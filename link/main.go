// Lucon Link — runs on the laptop next to Lucon Studio.
// It takes the programme from Lucon Studio and sends it to YouTube, Facebook, Instagram and other RTMP services,
// makes professional recordings (ProRes, DNxHR, MPEG-2) and controls PTZ cameras.
// It only listens on this computer (127.0.0.1) and only accepts Lucon Studio and its own window.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "1.3.0"

//go:embed ui.html
var uiHTML []byte

type Dest struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"` // youtube | facebook | instagram | custom
	URL     string `json:"url"`
	Key     string `json:"key,omitempty"`
	Enabled bool   `json:"enabled"`
}

type Config struct {
	Dests      []Dest   `json:"dests"`
	RecFormat  string   `json:"recFormat"`
	RecFolder  string   `json:"recFolder"`
	PTZ        []PTZCam `json:"ptz"`
	FFmpeg     string   `json:"ffmpeg,omitempty"`
	StreamMode string   `json:"streamMode"` // auto | encode
	Origins    []string `json:"origins,omitempty"`
}

var (
	cfgMu    sync.Mutex
	cfg      Config
	cfgPath  string
	port     = 9110
	lastSeen time.Time // last time Lucon Studio asked for status
)

func configDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d, _ = os.UserHomeDir()
	}
	return filepath.Join(d, "LuconLink")
}

func defaultFolder() string {
	h, _ := os.UserHomeDir()
	sub := "Videos"
	if runtime.GOOS == "darwin" {
		sub = "Movies"
	}
	return filepath.Join(h, sub, "Lucon Link")
}

func loadConfig() {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	b, err := os.ReadFile(cfgPath)
	if err == nil {
		json.Unmarshal(b, &cfg)
	}
	if cfg.RecFormat == "" {
		cfg.RecFormat = "prores422hq"
	}
	if cfg.RecFolder == "" {
		cfg.RecFolder = defaultFolder()
	}
	if cfg.StreamMode == "" {
		cfg.StreamMode = "auto"
	}
}

func saveConfigLocked() error {
	os.MkdirAll(filepath.Dir(cfgPath), 0o700)
	b, _ := json.MarshalIndent(cfg, "", "  ")
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cfgPath)
}

// maskKeys hides every saved stream key in text shown to anyone.
func maskKeys(s string) string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	for _, d := range cfg.Dests {
		if k := strings.TrimSpace(d.Key); len(k) >= 4 {
			s = strings.ReplaceAll(s, k, "••••")
		}
	}
	return s
}

/* ---------------- FFmpeg ---------------- */

var ff struct {
	sync.Mutex
	path, ver, enc string
	prores         string
	installing     bool
	installLog     string
}

func ffmpegPath() string { ff.Lock(); defer ff.Unlock(); return ff.path }

func hwEncoder() string {
	ff.Lock()
	defer ff.Unlock()
	if ff.enc == "" {
		return "libx264"
	}
	return ff.enc
}

func candidatesFFmpeg() []string {
	exe := "ffmpeg"
	if runtime.GOOS == "windows" {
		exe = "ffmpeg.exe"
	}
	var c []string
	cfgMu.Lock()
	if cfg.FFmpeg != "" {
		c = append(c, cfg.FFmpeg)
	}
	cfgMu.Unlock()
	if me, err := os.Executable(); err == nil {
		c = append(c, filepath.Join(filepath.Dir(me), exe))
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		c = append(c, p)
	}
	switch runtime.GOOS {
	case "windows":
		la := os.Getenv("LOCALAPPDATA")
		c = append(c, filepath.Join(la, "Microsoft", "WinGet", "Links", "ffmpeg.exe"))
		m, _ := filepath.Glob(filepath.Join(la, "Microsoft", "WinGet", "Packages", "Gyan.FFmpeg*", "*", "bin", "ffmpeg.exe"))
		sort.Sort(sort.Reverse(sort.StringSlice(m)))
		c = append(c, m...)
	case "darwin":
		c = append(c, "/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg")
	}
	return c
}

func runQuiet(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func findFFmpeg() {
	for _, p := range candidatesFFmpeg() {
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			continue
		}
		out, err := runQuiet(10*time.Second, p, "-hide_banner", "-version")
		if err != nil {
			continue
		}
		v := strings.SplitN(out, "\n", 2)[0]
		ff.Lock()
		ff.path, ff.ver = p, strings.TrimSpace(v)
		ff.Unlock()
		log.Printf("FFmpeg found: %s", p)
		go detectEncoder(p)
		return
	}
	ff.Lock()
	ff.path, ff.ver = "", ""
	ff.Unlock()
	log.Printf("FFmpeg not found yet. Open the Lucon Link window and click Install FFmpeg.")
}

// detectEncoder picks the laptop's graphics-chip encoder if it works, else the standard one.
func detectEncoder(p string) {
	if e := os.Getenv("LUCON_LINK_ENCODER"); e != "" {
		ff.Lock()
		ff.enc = e
		ff.Unlock()
		return
	}
	var cands []string
	switch runtime.GOOS {
	case "windows":
		cands = []string{"h264_nvenc", "h264_qsv", "h264_amf"}
	case "darwin":
		cands = []string{"h264_videotoolbox"}
	default:
		cands = []string{"h264_nvenc", "h264_qsv"}
	}
	enc := "libx264"
	for _, c := range cands {
		if _, err := runQuiet(12*time.Second, p, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=640x360:r=30", "-frames:v", "5", "-c:v", c, "-f", "null", "-"); err == nil {
			enc = c
			break
		}
	}
	prores := ""
	if runtime.GOOS == "darwin" {
		// Macs can make ProRes on the graphics chip; use it only if a real test works
		if _, err := runQuiet(15*time.Second, p, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=1280x720:r=25", "-frames:v", "5", "-c:v", "prores_videotoolbox", "-profile:v", "hq", "-f", "null", "-"); err == nil {
			prores = "prores_videotoolbox"
		}
	}
	ff.Lock()
	ff.enc = enc
	ff.prores = prores
	ff.Unlock()
	log.Printf("Video encoder: %s %s", enc, prores)
}

func proresEncoder() string { ff.Lock(); defer ff.Unlock(); return ff.prores }

func installFFmpeg() {
	ff.Lock()
	if ff.installing {
		ff.Unlock()
		return
	}
	ff.installing, ff.installLog = true, "Installing FFmpeg… this takes about 2 minutes."
	ff.Unlock()
	go func() {
		out, err := runQuiet(15*time.Minute, "winget", "install", "--id", "Gyan.FFmpeg", "-e", "--silent", "--accept-source-agreements", "--accept-package-agreements")
		msg := "FFmpeg installed."
		if err != nil {
			msg = "The automatic install did not finish. " + lastLines(out, 3)
		}
		findFFmpeg()
		ff.Lock()
		ff.installing = false
		if ff.path == "" && err == nil {
			msg = "FFmpeg was installed. Close Lucon Link and open it again."
		}
		ff.installLog = msg
		ff.Unlock()
	}()
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.TrimSpace(strings.Join(l, " "))
}

/* ---------------- security: only this computer, only Lucon Studio ---------------- */

func allowedOrigins() []string {
	o := []string{"https://events.luconhouse.com"}
	cfgMu.Lock()
	o = append(o, cfg.Origins...)
	cfgMu.Unlock()
	if e := os.Getenv("LUCON_LINK_ORIGINS"); e != "" {
		o = append(o, strings.Split(e, ",")...)
	}
	return o
}

func selfOrigin(o string) bool {
	return o == fmt.Sprintf("http://127.0.0.1:%d", port) || o == fmt.Sprintf("http://localhost:%d", port)
}

func studioOrigin(o string) bool {
	for _, a := range allowedOrigins() {
		if strings.TrimRight(strings.TrimSpace(a), "/") == o {
			return true
		}
	}
	return false
}

func hostOK(r *http.Request) bool {
	h := r.Host
	return h == fmt.Sprintf("127.0.0.1:%d", port) || h == fmt.Sprintf("localhost:%d", port)
}

// guard checks the request and sets the headers browsers need. who: "self" (Lucon Link window only) or "studio" (also Lucon Studio).
func guard(w http.ResponseWriter, r *http.Request, who string) bool {
	if !hostOK(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	o := r.Header.Get("Origin")
	// the Lucon Link window reading its own page sends no Origin on GET; other sites cannot read the reply
	ok := selfOrigin(o) || (o == "" && r.Method == http.MethodGet)
	if who == "studio" && studioOrigin(o) {
		ok = true
	}
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	if o != "" {
		w.Header().Set("Access-Control-Allow-Origin", o)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

/* ---------------- HTTP ---------------- */

func stateFor(full bool) map[string]any {
	cfgMu.Lock()
	dests := []map[string]any{}
	for _, d := range cfg.Dests {
		m := map[string]any{"id": d.ID, "name": d.Name, "type": d.Type, "enabled": d.Enabled, "ready": strings.TrimSpace(d.URL) != "" && strings.TrimSpace(d.Key) != ""}
		if full {
			m["url"] = d.URL
			if k := strings.TrimSpace(d.Key); k != "" {
				tail := ""
				if len(k) > 8 {
					tail = k[len(k)-4:]
				}
				m["keyHint"] = "••••" + tail
			}
		}
		dests = append(dests, m)
	}
	ptz := []map[string]any{}
	for _, c := range cfg.PTZ {
		m := map[string]any{"id": c.ID, "name": c.Name, "presets": c.Presets}
		if full {
			m["ip"], m["proto"], m["port"] = c.IP, c.Proto, c.Port
		}
		ptz = append(ptz, m)
	}
	formats := []map[string]string{}
	for _, f := range recFormats {
		formats = append(formats, map[string]string{"id": f[0], "label": f[1]})
	}
	st := map[string]any{"app": "lucon-link", "version": version, "features": []string{"hqfeed", "uhd"}, "os": runtime.GOOS, "dests": dests, "ptz": ptz, "recFormat": cfg.RecFormat, "recFormats": formats, "streamMode": cfg.StreamMode}
	if full {
		st["recFolder"] = cfg.RecFolder
		st["ffmpegSetting"] = cfg.FFmpeg
		st["studioSeen"] = !lastSeen.IsZero() && time.Since(lastSeen) < 15*time.Second
	}
	cfgMu.Unlock()
	wsp.Lock()
	st["whisper"] = map[string]any{"state": wsp.state, "model": filepath.Base(wsp.model)}
	if wsp.state != "missing" && wsp.state != "" {
		st["features"] = []string{"hqfeed", "uhd", "whisper"}
	}
	wsp.Unlock()
	ff.Lock()
	st["ffmpeg"] = map[string]any{"ok": ff.path != "", "version": ff.ver, "encoder": ff.enc, "installing": ff.installing, "log": ff.installLog}
	if full {
		st["ffmpeg"].(map[string]any)["path"] = ff.path
	}
	ff.Unlock()
	live.Lock()
	if live.s != nil {
		st["live"] = live.s.Status()
	}
	live.Unlock()
	return st
}

func handleState(w http.ResponseWriter, r *http.Request) {
	if !guard(w, r, "studio") {
		return
	}
	full := selfOrigin(r.Header.Get("Origin")) || r.Header.Get("Origin") == ""
	if !full {
		cfgMu.Lock()
		lastSeen = time.Now()
		cfgMu.Unlock()
	}
	writeJSON(w, stateFor(full))
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	if !guard(w, r, "self") {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var in Config
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	live.Lock()
	busy := live.s != nil
	live.Unlock()
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if busy {
		w.WriteHeader(409)
		writeJSON(w, map[string]string{"error": "End the broadcast in Lucon Studio before changing settings."})
		return
	}
	old := map[string]string{}
	for _, d := range cfg.Dests {
		old[d.ID] = d.Key
	}
	for i := range in.Dests {
		d := &in.Dests[i]
		d.Name, d.URL, d.Key = strings.TrimSpace(d.Name), strings.TrimSpace(d.URL), strings.TrimSpace(d.Key)
		if d.Key == "" {
			d.Key = old[d.ID]
		}
		if d.URL != "" {
			u, err := url.Parse(d.URL)
			if err != nil || (u.Scheme != "rtmp" && u.Scheme != "rtmps") || u.Host == "" {
				w.WriteHeader(400)
				writeJSON(w, map[string]string{"error": "“" + d.Name + "”: the server address must start with rtmp:// or rtmps://"})
				return
			}
		}
		if d.ID == "" {
			d.ID = strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.Itoa(i)
		}
	}
	for i := range in.PTZ {
		c := &in.PTZ[i]
		c.Name, c.IP = strings.TrimSpace(c.Name), strings.TrimSpace(c.IP)
		if c.IP != "" && net.ParseIP(c.IP) == nil {
			w.WriteHeader(400)
			writeJSON(w, map[string]string{"error": "“" + c.Name + "”: type the camera’s IP address, for example 192.168.1.60"})
			return
		}
		if c.Proto != "udp" && c.Proto != "tcp" {
			c.Proto = "sony"
		}
		if c.ID == "" {
			c.ID = strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.Itoa(i)
		}
	}
	valid := false
	for _, f := range recFormats {
		if f[0] == in.RecFormat {
			valid = true
		}
	}
	if !valid {
		in.RecFormat = "prores422hq"
	}
	if in.StreamMode != "encode" {
		in.StreamMode = "auto"
	}
	cfg.Dests, cfg.PTZ, cfg.RecFormat, cfg.StreamMode = in.Dests, in.PTZ, in.RecFormat, in.StreamMode
	if strings.TrimSpace(in.RecFolder) != "" {
		cfg.RecFolder = strings.TrimSpace(in.RecFolder)
	}
	if err := saveConfigLocked(); err != nil {
		w.WriteHeader(500)
		writeJSON(w, map[string]string{"error": "Could not save the settings: " + err.Error()})
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func handleFFmpeg(w http.ResponseWriter, r *http.Request) {
	if !guard(w, r, "self") || r.Method != http.MethodPost {
		return
	}
	var in struct {
		Action, Path string
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in)
	switch in.Action {
	case "install":
		if runtime.GOOS != "windows" {
			writeJSON(w, map[string]string{"error": "On a Mac, open Terminal and type: brew install ffmpeg — then click Check again."})
			return
		}
		installFFmpeg()
	case "path":
		cfgMu.Lock()
		cfg.FFmpeg = strings.TrimSpace(in.Path)
		saveConfigLocked()
		cfgMu.Unlock()
		findFFmpeg()
	default:
		findFFmpeg()
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func handleFolder(w http.ResponseWriter, r *http.Request) {
	if !guard(w, r, "self") || r.Method != http.MethodPost {
		return
	}
	var in struct{ Action string }
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&in)
	cfgMu.Lock()
	folder := cfg.RecFolder
	cfgMu.Unlock()
	if in.Action == "open" {
		os.MkdirAll(folder, 0o755)
		openPath(folder)
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	var out string
	var err error
	switch runtime.GOOS {
	case "windows":
		out, err = runQuiet(10*time.Minute, "powershell", "-NoProfile", "-STA", "-Command",
			"Add-Type -AssemblyName System.Windows.Forms; $f=New-Object System.Windows.Forms.FolderBrowserDialog; $f.Description='Choose where Lucon Link saves recordings'; $w=New-Object System.Windows.Forms.Form; $w.TopMost=$true; if($f.ShowDialog($w) -eq 'OK'){ [Console]::Out.Write($f.SelectedPath) }")
	case "darwin":
		out, err = runQuiet(10*time.Minute, "osascript", "-e", `POSIX path of (choose folder with prompt "Choose where Lucon Link saves recordings")`)
	default:
		out, err = runQuiet(10*time.Minute, "zenity", "--file-selection", "--directory")
	}
	p := strings.TrimSpace(out)
	if err != nil || p == "" {
		writeJSON(w, map[string]string{"folder": ""})
		return
	}
	writeJSON(w, map[string]string{"folder": p})
}

func handlePTZ(w http.ResponseWriter, r *http.Request) {
	if !guard(w, r, "studio") || r.Method != http.MethodPost {
		return
	}
	var in struct {
		Cam, Cmd, Dir, Speed string
		N                    int
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&in); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	cfgMu.Lock()
	var cam *PTZCam
	for i := range cfg.PTZ {
		if cfg.PTZ[i].ID == in.Cam {
			c := cfg.PTZ[i]
			cam = &c
		}
	}
	cfgMu.Unlock()
	if cam == nil {
		w.WriteHeader(404)
		writeJSON(w, map[string]string{"error": "That camera is not in Lucon Link."})
		return
	}
	b, err := ptzCommand(in.Cmd, in.Dir, in.Speed, in.N)
	if err == nil {
		err = ptzSend(*cam, b)
	}
	if err != nil {
		w.WriteHeader(502)
		writeJSON(w, map[string]string{"error": cam.Name + ": " + err.Error()})
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func handleLive(w http.ResponseWriter, r *http.Request) {
	if !hostOK(r) {
		http.Error(w, "forbidden", 403)
		return
	}
	o := r.Header.Get("Origin")
	if !studioOrigin(o) && !selfOrigin(o) {
		http.Error(w, "forbidden", 403)
		return
	}
	ws, err := wsUpgrade(w, r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	defer ws.Close()
	fail := func(msg string) {
		b, _ := json.Marshal(map[string]string{"type": "error", "error": msg})
		ws.WriteText(string(b))
	}
	q := r.URL.Query()
	num := func(k string) int { v, _ := strconv.Atoi(q.Get(k)); return v }
	req := liveReq{Mime: q.Get("mime"), Kbps: num("kbps"), FPS: num("fps"), W: num("w"), H: num("h"), Rec: q.Get("rec") == "1", Name: q.Get("name"), HQ: q.Get("hq") == "1", SKbps: num("skbps"), SW: num("sw"), SH: num("sh")}
	want := map[string]bool{}
	for _, id := range strings.Split(q.Get("dests"), ",") {
		if id != "" {
			want[id] = true
		}
	}
	cfgMu.Lock()
	for _, d := range cfg.Dests {
		if want[d.ID] && d.Enabled && d.URL != "" && d.Key != "" {
			req.Dests = append(req.Dests, d)
		}
	}
	req.RecFormat, req.RecFolder, req.ForceEnc = cfg.RecFormat, cfg.RecFolder, cfg.StreamMode == "encode"
	cfgMu.Unlock()
	if len(req.Dests) == 0 && !req.Rec {
		fail("Choose at least one place to stream, or the professional recording.")
		return
	}
	if req.Rec {
		if err := os.MkdirAll(req.RecFolder, 0o755); err != nil {
			fail("Lucon Link cannot save in the recording folder. Choose another folder in Lucon Link.")
			return
		}
	}
	live.Lock()
	if live.s != nil {
		live.Unlock()
		fail("A broadcast is already running from another window.")
		return
	}
	s, err := startLive(req)
	if err != nil {
		live.Unlock()
		fail(err.Error())
		return
	}
	live.s = s
	live.Unlock()
	log.Printf("Broadcast started: %d destination(s), recording %v", len(req.Dests), req.Rec)
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				b, _ := json.Marshal(map[string]any{"type": "status", "status": s.Status()})
				if ws.WriteText(string(b)) != nil {
					return
				}
			}
		}
	}()
	for {
		op, data, err := ws.ReadMessage(32 << 20)
		if err != nil {
			break
		}
		if op == 2 {
			if err := s.Write(data); err != nil {
				s.mu.Lock()
				m := s.InMsg
				s.mu.Unlock()
				fail("Lucon Link could not read the picture from the Studio. " + m)
				break
			}
		} else if op == 1 {
			var c struct{ Cmd string }
			json.Unmarshal(data, &c)
			if c.Cmd == "end" {
				break
			}
		}
	}
	s.End()
	close(stop)
	b, _ := json.Marshal(map[string]any{"type": "ended", "status": s.Status()})
	ws.WriteText(string(b))
	live.Lock()
	live.s = nil
	live.Unlock()
	log.Printf("Broadcast ended.")
}

func handleUI(w http.ResponseWriter, r *http.Request) {
	if !hostOK(r) {
		http.Error(w, "forbidden", 403)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Write(uiHTML)
}

func openPath(p string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		if strings.HasPrefix(p, "http") {
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", p)
		} else {
			cmd = exec.Command("explorer", p)
		}
	case "darwin":
		cmd = exec.Command("open", p)
	default:
		cmd = exec.Command("xdg-open", p)
	}
	cmd.Start()
}

func main() {
	noBrowser := flag.Bool("no-browser", false, "do not open the window")
	flag.IntVar(&port, "port", 9110, "port on this computer")
	cfgFile := flag.String("config", "", "settings file")
	flag.StringVar(&whisperDir, "whisper", "", "folder with whisper-server and models (offline captions)")
	appMode := flag.Bool("app", false, "started by the Lucon Studio app: stop when the app closes")
	flag.Parse()
	log.SetFlags(log.Ltime)
	cfgPath = *cfgFile
	if cfgPath == "" {
		cfgPath = filepath.Join(configDir(), "config.json")
	}
	loadConfig()
	fmt.Printf("\n  LUCON LINK %s\n  Keep this window open during your event. Closing it stops streaming.\n  Settings window: http://127.0.0.1:%d\n\n", version, port)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// already running: just show its window
		log.Printf("Lucon Link is already running. Opening its window.")
		if !*noBrowser {
			openPath(fmt.Sprintf("http://127.0.0.1:%d/", port))
		}
		time.Sleep(3 * time.Second)
		return
	}
	go findFFmpeg()
	findWhisper()
	go whisperIdle()
	stopAll := func() { whisperStop(); os.Exit(0) }
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; stopAll() }()
	if *appMode {
		// the app keeps our input open; when the app closes (or crashes) it closes, and we stop too
		go func() { io.Copy(io.Discard, os.Stdin); log.Printf("The Lucon Studio app closed. Stopping."); live.Lock(); if live.s != nil { live.s.End() }; live.Unlock(); stopAll() }()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleUI)
	mux.HandleFunc("/api/state", handleState)
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/ffmpeg", handleFFmpeg)
	mux.HandleFunc("/api/folder", handleFolder)
	mux.HandleFunc("/api/ptz", handlePTZ)
	mux.HandleFunc("/ws/live", handleLive)
	mux.HandleFunc("/api/whisper", handleWhisper)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if !*noBrowser {
		go func() { time.Sleep(700 * time.Millisecond); openPath(fmt.Sprintf("http://127.0.0.1:%d/", port)) }()
	}
	log.Fatal(srv.Serve(ln))
}
