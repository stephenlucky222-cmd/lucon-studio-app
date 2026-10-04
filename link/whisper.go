package main

// Offline captions: Lucon Studio sends short pieces of speech (16 kHz mono, 16-bit PCM) to Lucon Link,
// and Lucon Link turns them into words with Whisper (whisper.cpp), running on this computer.
// No sound leaves the computer. Whisper runs as a helper program ("whisper-server") that Lucon Link
// starts the first time it is needed, on a private port, and stops when Lucon Link stops.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

var whisperDir string // folder with whisper-server and models/ (set by -whisper, or found next to Lucon Link)

var wsp struct {
	sync.Mutex
	cmd     *exec.Cmd
	port    int
	model   string
	state   string // missing | idle | starting | ready | failed
	err     string
	started time.Time
	bin     int        // which build works on this computer
	busy    sync.Mutex // one piece of speech at a time
	last    time.Time
}

// whisperBins: the fast build first; on Windows also a "compat" build for older processors (no AVX2).
func whisperBins() []string {
	out := []string{}
	for _, n := range []string{"whisper-server", "whisper-server-compat"} {
		if runtime.GOOS == "windows" {
			n += ".exe"
		}
		p := filepath.Join(whisperDir, n)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func whisperBin() string {
	b := whisperBins()
	if len(b) == 0 {
		return filepath.Join(whisperDir, "whisper-server")
	}
	if wsp.bin >= len(b) {
		wsp.bin = len(b) - 1
	}
	return b[wsp.bin]
}

func findWhisper() {
	if whisperDir == "" {
		if exe, err := os.Executable(); err == nil {
			d := filepath.Dir(exe)
			for _, c := range []string{filepath.Join(d, "whisper"), filepath.Join(d, "..", "whisper"), filepath.Join(d, "..", "Resources", "whisper")} {
				if _, err := os.Stat(c); err == nil {
					whisperDir = c
					break
				}
			}
		}
	}
	wsp.Lock()
	defer wsp.Unlock()
	if whisperDir == "" || len(whisperBins()) == 0 {
		wsp.state = "missing"
		return
	}
	wsp.model = pickModel("")
	if wsp.model == "" {
		wsp.state = "missing"
		return
	}
	wsp.state = "idle"
}

// models: the larger "small" model on fast computers (Apple chips), the "base" model elsewhere.
func listModels() []string {
	m, _ := filepath.Glob(filepath.Join(whisperDir, "models", "ggml-*.bin"))
	sort.Strings(m)
	return m
}

func pickModel(want string) string {
	ms := listModels()
	find := func(s string) string {
		for _, m := range ms {
			if strings.Contains(filepath.Base(m), s) {
				return m
			}
		}
		return ""
	}
	if want != "" {
		if m := find(want); m != "" {
			return m
		}
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		if m := find("small"); m != "" {
			return m
		}
	}
	if m := find("base"); m != "" {
		return m
	}
	if len(ms) > 0 {
		return ms[0]
	}
	return ""
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 9111
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// whisperStart starts the helper and waits until the model is loaded (a few seconds the first time).
func whisperStart() error {
	wsp.Lock()
	if wsp.state == "ready" && wsp.cmd != nil && wsp.cmd.ProcessState == nil {
		wsp.Unlock()
		return nil
	}
	if wsp.state == "missing" {
		wsp.Unlock()
		return errors.New("Offline captions are not installed with this Lucon Link. Use the Lucon Studio app.")
	}
	if wsp.state == "starting" {
		wsp.Unlock()
		for i := 0; i < 600; i++ {
			time.Sleep(100 * time.Millisecond)
			wsp.Lock()
			s, e := wsp.state, wsp.err
			wsp.Unlock()
			if s == "ready" {
				return nil
			}
			if s == "failed" || s == "idle" {
				return errors.New(e)
			}
		}
		return errors.New("Whisper is taking too long to start.")
	}
	wsp.state, wsp.err, wsp.port = "starting", "", freePort()
	threads := runtime.NumCPU() - 1
	if threads > 8 {
		threads = 8
	}
	if threads < 2 {
		threads = 2
	}
	args := []string{"--host", "127.0.0.1", "--port", fmt.Sprint(wsp.port), "-m", wsp.model, "-t", fmt.Sprint(threads), "-nt", "-l", "en"}
	cmd := exec.Command(whisperBin(), args...)
	cmd.Dir = whisperDir
	hideWindow(cmd)
	var logb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logb, &logb
	if err := cmd.Start(); err != nil {
		wsp.state, wsp.err = "failed", "Whisper could not start: "+err.Error()
		wsp.Unlock()
		return errors.New(wsp.err)
	}
	wsp.cmd, wsp.started = cmd, time.Now()
	port := wsp.port
	model := filepath.Base(wsp.model)
	wsp.Unlock()
	log.Printf("Offline captions: starting Whisper (%s)", model)
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
		wsp.Lock()
		if wsp.cmd == cmd {
			if wsp.state == "starting" {
				wsp.state, wsp.err = "failed", "Whisper stopped while starting. "+lastLine(logb.String())
			} else {
				wsp.state = "idle"
			}
			wsp.cmd = nil
		}
		wsp.Unlock()
	}()
	for i := 0; i < 1200; i++ { // up to 2 minutes (a slow computer loading a large model)
		select {
		case <-done:
			wsp.Lock()
			e := wsp.err
			retry := time.Since(wsp.started) < 20*time.Second && wsp.bin+1 < len(whisperBins())
			if retry {
				wsp.bin++ // this processor cannot run the fast build: use the compatible one
				wsp.state = "idle"
				log.Printf("Offline captions: trying the compatible Whisper for this processor")
			}
			wsp.Unlock()
			if retry {
				return whisperStart()
			}
			return errors.New(e)
		case <-time.After(100 * time.Millisecond):
		}
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			wsp.Lock()
			if wsp.cmd == cmd {
				wsp.state = "ready"
			}
			wsp.Unlock()
			log.Printf("Offline captions: Whisper is ready")
			return nil
		}
	}
	whisperStop()
	return errors.New("Whisper did not start in time.")
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 200 {
		s = s[len(s)-200:]
	}
	return s
}

func whisperStop() {
	wsp.Lock()
	defer wsp.Unlock()
	if wsp.cmd != nil && wsp.cmd.Process != nil {
		wsp.cmd.Process.Kill()
	}
	wsp.cmd = nil
	if wsp.state != "missing" {
		wsp.state = "idle"
	}
}

// pcmWAV wraps 16 kHz mono 16-bit samples in a WAV file.
func pcmWAV(pcm []byte) []byte {
	var b bytes.Buffer
	n := uint32(len(pcm))
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, 36+n)
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint32(16000))
	binary.Write(&b, binary.LittleEndian, uint32(32000))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, n)
	b.Write(pcm)
	return b.Bytes()
}

var whisperClient = &http.Client{Timeout: 60 * time.Second}

func whisperRun(pcm []byte, lang, prompt string) (string, error) {
	if err := whisperStart(); err != nil {
		return "", err
	}
	wsp.Lock()
	port := wsp.port
	wsp.Unlock()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "speech.wav")
	fw.Write(pcmWAV(pcm))
	field := func(k, v string) { mw.WriteField(k, v) }
	field("response_format", "json")
	field("temperature", "0.0")
	field("temperature_inc", "0.2")
	field("language", lang)
	field("no_timestamps", "true")
	field("suppress_nst", "true")
	if prompt != "" {
		field("prompt", prompt)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/inference", port), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := whisperClient.Do(req)
	if err != nil {
		whisperStop()
		return "", errors.New("Whisper did not answer. It will start again.")
	}
	defer res.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out struct {
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	json.Unmarshal(rb, &out)
	if res.StatusCode != 200 || out.Error != "" {
		if out.Error == "" {
			out.Error = fmt.Sprintf("Whisper answered %d", res.StatusCode)
		}
		return "", errors.New(out.Error)
	}
	return strings.TrimSpace(out.Text), nil
}

// POST /api/whisper?lang=en&prompt=… with the raw speech (16 kHz mono, 16-bit little-endian). GET = status.
func handleWhisper(w http.ResponseWriter, r *http.Request) {
	if !guard(w, r, "studio") {
		return
	}
	wsp.Lock()
	st, model, e := wsp.state, filepath.Base(wsp.model), wsp.err
	wsp.Unlock()
	if r.Method == http.MethodGet {
		if r.URL.Query().Get("warm") == "1" && (st == "idle" || st == "failed") {
			go whisperStart()
			st = "starting"
		}
		writeJSON(w, map[string]any{"state": st, "model": model, "error": e, "version": version})
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	pcm, err := io.ReadAll(io.LimitReader(r.Body, 32000*31))
	if err != nil || len(pcm) < 3200 {
		w.WriteHeader(400)
		writeJSON(w, map[string]string{"error": "Too little sound."})
		return
	}
	if len(pcm)%2 == 1 {
		pcm = pcm[:len(pcm)-1]
	}
	q := r.URL.Query()
	lang := q.Get("lang")
	if lang != "fr" && lang != "en" {
		lang = "en"
	}
	prompt := q.Get("prompt")
	if len(prompt) > 400 {
		prompt = prompt[len(prompt)-400:]
	}
	wsp.busy.Lock()
	t0 := time.Now()
	text, err := whisperRun(pcm, lang, prompt)
	wsp.busy.Unlock()
	if err != nil {
		w.WriteHeader(503)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	wsp.Lock()
	wsp.last = time.Now()
	wsp.Unlock()
	writeJSON(w, map[string]any{"text": text, "ms": time.Since(t0).Milliseconds(), "secs": float64(len(pcm)) / 32000})
}

// stop Whisper after 20 minutes without speech, to give the memory back
func whisperIdle() {
	for range time.Tick(time.Minute) {
		wsp.Lock()
		idle := wsp.state == "ready" && !wsp.last.IsZero() && time.Since(wsp.last) > 20*time.Minute
		wsp.Unlock()
		if idle {
			log.Printf("Offline captions: resting Whisper (not used for 20 minutes)")
			whisperStop()
		}
	}
}
