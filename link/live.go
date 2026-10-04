package main

// The live pipeline:
//   Lucon Studio ──(WebSocket: WebM/Matroska/MP4 pieces)──► ingest FFmpeg ──(MPEG-TS)──► one FFmpeg per destination
//                                                                                   └──► FFmpeg for the professional recording
// Each destination reconnects by itself if the platform drops it.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Out struct {
	ID, Name, Kind string // Kind: dest | rec
	args           func(part int) ([]string, string)
	ch             chan []byte
	mu             sync.Mutex
	State, Msg     string
	Kbps           int
	File           string
	part           int
	dropped        int64
	restarts       int
	stdin          io.WriteCloser
	done           chan struct{}
	sinks          []*Out // relay only: outputs fed from this FFmpeg's output
	fromRelay      bool   // fed by the relay instead of the main feed
}

type LiveSession struct {
	mu       sync.Mutex
	Since    time.Time
	ingest   *exec.Cmd
	ingestIn io.WriteCloser
	outs     []*Out
	inBytes  int64
	ending   atomic.Bool
	ended    chan struct{}
	InMsg    string
	fps      int
}

var live struct {
	sync.Mutex
	s *LiveSession
}

var reBitrate = regexp.MustCompile(`bitrate=\s*([0-9.]+)kbits/s`)

type liveReq struct {
	Mime      string
	Kbps, FPS int
	W, H      int
	Dests     []Dest
	Rec       bool
	RecFormat string
	RecFolder string
	Name      string
	ForceEnc  bool
	HQ        bool // the Studio sends a top-quality feed; platforms get a lighter copy made here
	SKbps     int
	SW, SH    int
}

func encoderArgs(kbps, fps int, forSize string) []string {
	g := strconv.Itoa(max(fps, 1) * 2)
	enc := hwEncoder()
	a := []string{"-c:v", enc}
	switch enc {
	case "libx264":
		a = append(a, "-preset", "veryfast")
	case "h264_nvenc":
		a = append(a, "-preset", "p4")
	case "h264_videotoolbox":
		a = append(a, "-realtime", "1")
	}
	k := strconv.Itoa(kbps) + "k"
	a = append(a, "-b:v", k, "-maxrate", k, "-bufsize", strconv.Itoa(kbps*2)+"k", "-g", g, "-keyint_min", g, "-pix_fmt", "yuv420p")
	if enc == "libx264" {
		a = append(a, "-sc_threshold", "0")
	}
	return a
}

func startLive(req liveReq) (*LiveSession, error) {
	ff := ffmpegPath()
	if ff == "" {
		return nil, fmt.Errorf("FFmpeg is not installed. Open Lucon Link and click Install FFmpeg")
	}
	fps := req.FPS
	if fps <= 0 || fps > 60 {
		fps = 30
	}
	kbps := req.Kbps
	if kbps < 500 || kbps > 120000 {
		kbps = 6000
	}
	stKbps := kbps
	if req.HQ && req.SKbps >= 500 && req.SKbps <= 20000 {
		stKbps = req.SKbps
	}
	s := &LiveSession{Since: time.Now(), ended: make(chan struct{}), fps: fps}
	var relay *Out
	needRelay := false
	for _, d := range req.Dests {
		if d.Type != "instagram" {
			needRelay = true
		}
	}
	if req.HQ && needRelay {
		relay = &Out{ID: "relay", Name: "Picture for the platforms", Kind: "relay", State: "starting", ch: make(chan []byte, 1024), done: make(chan struct{})}
		relay.args = func(int) ([]string, string) {
			a := []string{"-hide_banner", "-loglevel", "error", "-stats", "-f", "mpegts", "-i", "pipe:0"}
			if req.SW > 0 && req.SH > 0 && (req.SW != req.W || req.SH != req.H) {
				a = append(a, "-vf", fmt.Sprintf("scale=%d:%d", req.SW, req.SH))
			}
			a = append(a, encoderArgs(stKbps, fps, "")...)
			a = append(a, "-r", strconv.Itoa(fps), "-c:a", "copy", "-f", "mpegts", "-muxdelay", "0", "-mpegts_flags", "+resend_headers", "pipe:1")
			return a, ""
		}
		s.outs = append(s.outs, relay)
	}
	h264 := strings.Contains(strings.ToLower(req.Mime), "avc1") || strings.Contains(strings.ToLower(req.Mime), "h264")
	in := []string{"-hide_banner", "-loglevel", "error", "-fflags", "+genpts"}
	if strings.Contains(req.Mime, "webm") || strings.Contains(req.Mime, "matroska") {
		in = append(in, "-f", "matroska")
	}
	in = append(in, "-i", "pipe:0")
	if h264 && !req.ForceEnc {
		in = append(in, "-c:v", "copy")
	} else {
		in = append(in, encoderArgs(kbps, fps, "")...)
		in = append(in, "-r", strconv.Itoa(fps))
	}
	in = append(in, "-c:a", "aac", "-b:a", "160k", "-ar", "48000", "-ac", "2", "-f", "mpegts", "-muxdelay", "0", "-mpegts_flags", "+resend_headers", "pipe:1")
	cmd := exec.Command(ff, in...)
	hideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start FFmpeg: %v", err)
	}
	s.ingest, s.ingestIn = cmd, stdin
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			s.mu.Lock()
			s.InMsg = maskKeys(sc.Text())
			s.mu.Unlock()
		}
	}()

	for _, d := range req.Dests {
		d := d
		target := strings.TrimSpace(d.URL)
		if !strings.HasSuffix(target, "/") {
			target += "/"
		}
		target += strings.TrimSpace(d.Key)
		o := &Out{ID: d.ID, Name: d.Name, Kind: "dest", State: "connecting", ch: make(chan []byte, 512), done: make(chan struct{})}
		if relay != nil && d.Type != "instagram" {
			o.fromRelay = true
			relay.sinks = append(relay.sinks, o)
		}
		o.args = func(int) ([]string, string) {
			a := []string{"-hide_banner", "-loglevel", "error", "-stats", "-f", "mpegts", "-i", "pipe:0"}
			switch {
			case d.Type == "instagram":
				a = append(a, "-vf", "crop=trunc(ih*9/16/2)*2:ih,scale=720:1280,setsar=1")
				a = append(a, encoderArgs(3500, fps, "720x1280")...)
				a = append(a, "-r", strconv.Itoa(fps), "-c:a", "aac", "-b:a", "128k", "-ar", "48000")
			case d.Type == "facebook" && stKbps > 6000:
				a = append(a, encoderArgs(6000, fps, "")...)
				a = append(a, "-c:a", "copy")
			default:
				a = append(a, "-c", "copy")
			}
			a = append(a, "-f", "flv", "-flvflags", "no_duration_filesize", target)
			return a, ""
		}
		s.outs = append(s.outs, o)
	}
	if req.Rec {
		o := &Out{ID: "rec", Name: recLabel(req.RecFormat), Kind: "rec", State: "starting", ch: make(chan []byte, 2048), done: make(chan struct{})}
		stamp := time.Now().Format("2006-01-02_15-04")
		base := safeName(req.Name)
		if base == "" {
			base = "lucon"
		}
		o.args = func(part int) ([]string, string) {
			codec, ext := recArgs(req.RecFormat, req.W, req.H)
			name := fmt.Sprintf("%s-%s-%s", base, stamp, req.RecFormat)
			if part > 1 {
				name += fmt.Sprintf("-part%d", part)
			}
			file := filepath.Join(req.RecFolder, name+ext)
			a := []string{"-hide_banner", "-loglevel", "error", "-stats", "-f", "mpegts", "-i", "pipe:0", "-r", recRate(req.RecFormat, fps)}
			a = append(a, codec...)
			a = append(a, "-y", file)
			return a, file
		}
		s.outs = append(s.outs, o)
	}
	for _, o := range s.outs {
		go s.runOut(ff, o)
	}
	// fan the MPEG-TS out to every destination and the recorder
	go func() {
		buf := make([]byte, 188*350)
		for {
			n, err := io.ReadFull(stdout, buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				for _, o := range s.outs {
					if o.fromRelay {
						continue
					}
					select {
					case o.ch <- chunk:
					default:
						atomic.AddInt64(&o.dropped, 1)
					}
				}
			}
			if err != nil {
				break
			}
		}
		for _, o := range s.outs {
			if !o.fromRelay {
				close(o.ch)
			}
		}
		cmd.Wait()
		// platforms get 25 seconds to finish; a recording is waited for until it is complete
		// (a 4K ProRes file can take a little while to finish writing on a slower laptop)
		for _, o := range s.outs {
			wait := 25 * time.Second
			if o.Kind == "rec" {
				wait = 15 * time.Minute
			}
			select {
			case <-o.done:
			case <-time.After(wait):
			}
		}
		close(s.ended)
	}()
	return s, nil
}

func (s *LiveSession) runOut(ff string, o *Out) {
	defer close(o.done)
	if len(o.sinks) > 0 {
		defer func() {
			for _, sk := range o.sinks {
				close(sk.ch)
			}
		}()
	}
	for {
		o.mu.Lock()
		o.part++
		args, file := o.args(o.part)
		o.File = file
		o.mu.Unlock()
		cmd := exec.Command(ff, args...)
		hideWindow(cmd)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			o.set("error", err.Error())
			return
		}
		stderr, _ := cmd.StderrPipe()
		var outDone chan struct{}
		var stdout io.ReadCloser
		if len(o.sinks) > 0 {
			stdout, _ = cmd.StdoutPipe()
		}
		if err := cmd.Start(); err != nil {
			o.set("error", "could not start FFmpeg")
			return
		}
		if stdout != nil {
			outDone = make(chan struct{})
			go func() {
				defer close(outDone)
				buf := make([]byte, 188*350)
				for {
					n, err := io.ReadFull(stdout, buf)
					if n > 0 {
						chunk := make([]byte, n)
						copy(chunk, buf[:n])
						for _, sk := range o.sinks {
							select {
							case sk.ch <- chunk:
							default:
								atomic.AddInt64(&sk.dropped, 1)
							}
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
		go func() {
			r := bufio.NewReader(stderr)
			var line []byte
			for {
				b, err := r.ReadByte()
				if err != nil {
					break
				}
				if b == '\r' || b == '\n' {
					l := string(line)
					line = line[:0]
					if m := reBitrate.FindStringSubmatch(l); m != nil {
						v, _ := strconv.ParseFloat(m[1], 64)
						o.mu.Lock()
						o.Kbps = int(v)
						if o.State != "live" {
							o.State = "live"
							o.Msg = ""
						}
						o.mu.Unlock()
					} else if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "frame=") && !strings.HasPrefix(l, "size=") {
						o.mu.Lock()
						o.Msg = maskKeys(l)
						o.mu.Unlock()
					}
					continue
				}
				if len(line) < 2000 {
					line = append(line, b)
				}
			}
		}()
		// feed this output until the input ends or FFmpeg stops
		failed := false
		for chunk := range o.ch {
			if _, err := stdin.Write(chunk); err != nil {
				failed = true
				break
			}
		}
		stdin.Close()
		if outDone != nil {
			<-outDone
		}
		werr := cmd.Wait()
		o.mu.Lock()
		msg := o.Msg
		wasLive := o.State == "live"
		o.mu.Unlock()
		if !failed && werr == nil && (o.Kind != "rec" || fileHasData(file)) {
			o.set("finished", "")
			return
		}
		if msg == "" {
			if o.Kind == "rec" {
				msg = "The recording could not be written"
			} else {
				msg = "The connection dropped"
			}
		}
		if s.ending.Load() && (wasLive || o.Kind != "rec") {
			o.set("finished", "")
			return
		}
		o.mu.Lock()
		o.restarts++
		tries := o.restarts
		o.mu.Unlock()
		if o.Kind == "rec" {
			if file != "" && !fileHasData(file) {
				os.Remove(file)
			}
			if tries >= 3 || s.ending.Load() {
				o.set("error", msg)
				for range o.ch {
				}
				return
			}
		}
		o.set("retrying", msg)
		// drain what arrives while waiting, then try again
		t := time.After(3 * time.Second)
	wait:
		for {
			select {
			case _, ok := <-o.ch:
				if !ok {
					o.set("finished", "")
					return
				}
			case <-t:
				break wait
			}
		}
	}
}

func fileHasData(f string) bool {
	fi, err := os.Stat(f)
	return err == nil && fi.Size() > 0
}

func (o *Out) set(state, msg string) {
	o.mu.Lock()
	o.State, o.Msg = state, maskKeys(msg)
	o.mu.Unlock()
}

func (s *LiveSession) Write(p []byte) error {
	atomic.AddInt64(&s.inBytes, int64(len(p)))
	_, err := s.ingestIn.Write(p)
	return err
}

func (s *LiveSession) End() {
	if s.ending.Swap(true) {
		return
	}
	s.ingestIn.Close()
	select {
	case <-s.ended:
	case <-time.After(16 * time.Minute): // the recording is waited for (see startLive)
		if s.ingest.Process != nil {
			s.ingest.Process.Kill()
		}
	}
}

type outStatus struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Msg      string `json:"msg,omitempty"`
	Kbps     int    `json:"kbps"`
	Bytes    int64  `json:"bytes,omitempty"`
	File     string `json:"file,omitempty"`
	Dropped  int64  `json:"dropped"`
	Restarts int    `json:"restarts"`
}

func (s *LiveSession) Status() map[string]any {
	outs := []outStatus{}
	for _, o := range s.outs {
		if o.Kind == "relay" {
			o.mu.Lock()
			bad := o.State == "retrying" || o.State == "error"
			o.mu.Unlock()
			if !bad {
				continue
			}
		}
		o.mu.Lock()
		st := outStatus{ID: o.ID, Name: o.Name, Kind: o.Kind, State: o.State, Msg: o.Msg, Kbps: o.Kbps, Dropped: atomic.LoadInt64(&o.dropped), Restarts: o.restarts}
		if st.Dropped > 0 && st.Msg == "" {
			st.Msg = "This laptop is not keeping up, some video was skipped. Choose a lighter format or a lower stream quality."
		} else if o.Kind == "rec" && s.ending.Load() && o.State == "live" {
			st.Msg = "Finishing the recording… keep Lucon Link open"
		}
		if o.Kind == "rec" && o.File != "" {
			st.File = filepath.Base(o.File)
			if fi, err := os.Stat(o.File); err == nil {
				st.Bytes = fi.Size()
			}
		}
		o.mu.Unlock()
		outs = append(outs, st)
	}
	s.mu.Lock()
	im := s.InMsg
	s.mu.Unlock()
	return map[string]any{"since": s.Since.Unix(), "inBytes": atomic.LoadInt64(&s.inBytes), "outs": outs, "inMsg": im}
}

var recFormats = [][3]string{
	{"prores422hq", "Apple ProRes 422 HQ (.mov)", ""},
	{"prores422", "Apple ProRes 422 (.mov)", ""},
	{"dnxhr_hq", "Avid DNxHR HQ (.mov)", ""},
	{"mpeg2_50", "MPEG-2 4:2:2 50 Mb/s (.mxf)", ""},
	{"h264hq", "H.264 high quality (.mp4)", ""},
}

func recLabel(f string) string {
	for _, r := range recFormats {
		if r[0] == f {
			return "Pro recording · " + r[1]
		}
	}
	return "Pro recording"
}

func recRate(format string, fps int) string {
	if format == "mpeg2_50" {
		switch fps {
		case 24:
			return "24000/1001"
		case 30:
			return "30000/1001"
		case 60:
			return "60000/1001"
		}
	}
	return strconv.Itoa(fps)
}

func recArgs(f string, w, h int) ([]string, string) {
	uhd := w*h > 2560*1440
	pcm := []string{"-c:a", "pcm_s24le", "-ar", "48000"}
	if vt := proresEncoder(); vt != "" && (f == "prores422" || f == "prores422hq" || f == "") {
		prof := "hq"
		if f == "prores422" {
			prof = "standard"
		}
		return append([]string{"-c:v", vt, "-profile:v", prof}, pcm...), ".mov"
	}
	switch f {
	case "prores422":
		return append([]string{"-c:v", "prores_ks", "-profile:v", "2", "-vendor", "apl0", "-pix_fmt", "yuv422p10le"}, pcm...), ".mov"
	case "dnxhr_hq":
		return append([]string{"-c:v", "dnxhd", "-profile:v", "dnxhr_hq", "-pix_fmt", "yuv422p"}, pcm...), ".mov"
	case "mpeg2_50":
		// MPEG-2 4:2:2 (XDCAM HD422) is a 1080 format: a 4K picture is made 1920x1080 for it
		sc := []string{}
		if h > 1080 {
			sc = []string{"-vf", "scale=1920:1080"}
		}
		return append(append(sc, "-c:v", "mpeg2video", "-pix_fmt", "yuv422p", "-b:v", "50M", "-minrate", "50M", "-maxrate", "50M", "-bufsize", "17825792", "-g", "12", "-bf", "2", "-flags", "+cgop", "-sc_threshold", "1000000000", "-intra_vlc", "1", "-non_linear_quant", "1", "-qmin", "1", "-qmax", "28", "-f", "mxf"), pcm...), ".mxf"
	case "h264hq":
		enc := hwEncoder()
		a := []string{"-c:v", enc}
		if enc == "libx264" {
			a = append(a, "-preset", "fast", "-crf", "17")
		} else {
			if uhd {
				a = append(a, "-b:v", "80M")
			} else {
				a = append(a, "-b:v", "25M")
			}
		}
		return append(a, "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "256k", "-movflags", "+frag_keyframe+empty_moov+default_base_moof"), ".mp4"
	}
	return append([]string{"-c:v", "prores_ks", "-profile:v", "3", "-vendor", "apl0", "-pix_fmt", "yuv422p10le"}, pcm...), ".mov"
}

var reSafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func safeName(s string) string {
	s = strings.Trim(reSafe.ReplaceAllString(s, "-"), "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}
