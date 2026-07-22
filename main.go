package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// instance is one managed ET process with its own pipeline, telemetry and
// (for the live instance) director/watchdog/audio state. The agent runs one
// or two of these: the always-on "live" streaming instance and an optional
// on-demand "replay" instance. They share NO mutable state, so a replay
// failure can never disturb the live broadcast.
type instance struct {
	name       string // "live" | "replay" (logs only)
	homepath   string // fs_homepath; "" = ET default (live) or replay_homepath
	pipeAddr   string // this instance's cl_wtvPort pipe address
	directs    bool   // run the auto-director against this instance
	feedEvents bool   // feed this instance's actions/demos into the event feed

	mu            sync.Mutex
	etCmd         *exec.Cmd
	desired       bool
	servers       []string
	password      string
	overlayURL    string
	currentServer string
	startedAt     time.Time
	nameFails     int

	// pipeline
	pipe      net.Conn
	pipeUp    bool
	lastEvent time.Time
	tele      Telemetry
	discSince time.Time
	etDone    chan struct{}
	pipeCaps      map[string]bool
	clientVersion string

	// audio monitor
	audioMonUp    bool
	audioLevel    float64
	audioLastLoud time.Time
	winDefaultOut string
	freeSince     time.Time
	manualUntil   time.Time
	lastAudioFix  time.Time
	pending   map[int]chan []pipePlayer
	reqID     int

	// director
	specSent    bool
	activeSince time.Time
	nextSwitch  time.Time
	curTarget   string
	curTargetSlot int
}

// st is the LIVE instance. Existing call sites use it exactly as before; the
// only change is that it is now a pointer to an instance value.
var st = &instance{name: "live", directs: true, feedEvents: true}

// rp is the optional REPLAY instance (nil unless replay_enabled).
var rp *instance

/* ---------------- http ---------------- */

func auth(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+cfg.Token {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type startReq struct {
	Servers    []string `json:"servers"`
	Password   string   `json:"password"`
	OverlayURL string   `json:"overlay_url"`
}

func parseStartReq(w http.ResponseWriter, r *http.Request) (*startReq, bool) {
	var req startReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Servers) == 0 {
		writeJSON(w, 400, map[string]string{"error": `body must be {"servers":["ip:port",...]}`})
		return nil, false
	}
	return &req, true
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	req, ok := parseStartReq(w, r)
	if !ok {
		return
	}
	st.mu.Lock()
	if st.etCmd != nil {
		s := st.currentServer
		st.mu.Unlock()
		writeJSON(w, 409, map[string]string{"error": "already running", "server": s})
		return
	}
	target, err := deploy(req.Servers, req.Password, req.OverlayURL)
	if err != nil {
		st.mu.Unlock()
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	st.desired = true
	st.mu.Unlock()

	// pre-flight: is the right playback device the Windows default?
	if cfg.WinExpectDefault != "" {
		if name := winDefaultPlayback(); !winDeviceOK(name) {
			log.Printf("start: WARNING default playback is %q, expected %q -- stream may be SILENT",
				name, cfg.WinExpectDefault)
		}
		st.mu.Lock()
		st.winDefaultOut = winDefaultPlayback()
		st.mu.Unlock()
	}

	time.Sleep(8 * time.Second) // let ET create its window

	obs, err := ensureOBS()
	if err != nil {
		log.Println("start: obs error:", err)
		writeJSON(w, 500, map[string]string{"error": "obs: " + err.Error(), "server": target,
			"note": "ET running, stream NOT started"})
		return
	}
	defer obs.Disconnect()
	var serr error
	for i := 0; i < 10; i++ {
		_, serr = obs.Stream.StartStream()
		if serr == nil ||
			strings.Contains(strings.ToLower(serr.Error()), "already") ||
			strings.Contains(strings.ToLower(serr.Error()), "outputrunning") {
			serr = nil
			break
		}
		log.Println("start: stream not ready, retrying:", serr)
		time.Sleep(3 * time.Second)
	}
	if serr != nil {
		log.Println("start: stream error:", serr)
		writeJSON(w, 500, map[string]string{"error": "stream start: " + serr.Error(), "server": target})
		return
	}
	setOverlayURL(req.OverlayURL)
	log.Println("started, streaming on", target)
	writeJSON(w, 200, map[string]any{"ok": true, "server": target, "streaming": true})
}

func handleSwitch(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	req, ok := parseStartReq(w, r)
	if !ok {
		return
	}
	st.mu.Lock()
	target, err := deploy(req.Servers, req.Password, req.OverlayURL)
	if err == nil {
		st.desired = true
	}
	st.mu.Unlock()
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	log.Println("switched to", target)
	writeJSON(w, 200, map[string]any{"ok": true, "server": target})
}

// handleExec: raw console command passthrough (debug / future features).
func handleExec(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	var req struct {
		Line string `json:"line"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Line == "" {
		writeJSON(w, 400, map[string]string{"error": `body must be {"line":"..."}`})
		return
	}
	st.mu.Lock()
	ok := st.pipeExecLocked(req.Line)
	st.mu.Unlock()
	if !ok {
		writeJSON(w, 503, map[string]string{"error": "pipeline not connected"})
		return
	}
	log.Println("exec:", req.Line)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	st.mu.Lock()
	st.desired = false
	if cfg.KillObsOnStop {
		_ = exec.Command("taskkill", "/IM", "obs64.exe", "/F").Run()
	}
	st.kill()
	st.currentServer = ""
	st.mu.Unlock()

	// abort any in-flight replay so it does not linger against a stopped broadcast
	if rp != nil {
		replay.stop()
	}

	if obs, err := obsClient(); err == nil {
		_, _ = obs.Stream.StopStream()
		obs.Disconnect()
		log.Println("stream stopped")
	}
	log.Println("stopped")
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	st.mu.Lock()
	resp := map[string]any{
		"et_running": st.etCmd != nil,
		"server":     st.currentServer,
		"watchdog":   st.desired,
		"pipeline":   st.pipeUp,
		"telemetry":      st.tele,
		"following":      st.curTarget,
		"client_version": st.clientVersion,
		"audio": map[string]any{
			"monitor":    st.audioMonUp,
			"level":      st.audioLevel,
			"silent_sec": int(time.Since(st.audioLastLoud).Seconds()),
			"ok": st.audioMonUp && !st.audioLastLoud.IsZero() &&
				time.Since(st.audioLastLoud) < time.Duration(cfg.AudioSilenceSec)*time.Second,
			"default_device": st.winDefaultOut,
			"device_ok":      cfg.WinExpectDefault == "" || winDeviceOK(st.winDefaultOut),
		},
	}
	if st.etCmd != nil {
		resp["uptime_sec"] = int(time.Since(st.startedAt).Seconds())
	}
	st.mu.Unlock()

	obsUp, streaming := obsStreamStatus()
	resp["obs_up"] = obsUp
	resp["streaming"] = streaming
	writeJSON(w, 200, resp)
}

func handleLog(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	lines := 200
	if s := r.URL.Query().Get("lines"); s != "" {
		n := 0
		ok := true
		for _, c := range s {
			if c < '0' || c > '9' {
				ok = false
				break
			}
			n = n*10 + int(c-'0')
		}
		if ok && n > 0 && n <= 2000 {
			lines = n
		}
	}
	data, err := os.ReadFile(cfg.LogFile)
	if err != nil {
		writeJSON(w, 200, map[string]any{"log": "", "error": "no log file"})
		return
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	writeJSON(w, 200, map[string]any{"log": strings.Join(all, "\n")})
}

func handlePlayers(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	players, ok := queryPlayers(2 * time.Second)
	if !ok {
		writeJSON(w, 503, map[string]any{"error": "client query unavailable", "players": []any{}})
		return
	}
	// enrich with ping/bot info from getstatus (best effort)
	st.mu.Lock()
	server := st.currentServer
	curSlot := st.curTargetSlot
	st.mu.Unlock()
	pings := map[string]int{}
	if sp, alive := q3GetStatus(server); alive {
		for _, p := range sp {
			pings[strings.ToLower(cleanName(p.Name))] = p.Ping
		}
	}
	out := make([]map[string]any, 0, len(players))
	for _, p := range players {
		ping, hasPing := pings[strings.ToLower(cleanName(p.Name))]
		team := "spec"
		if p.Team == 1 {
			team = "axis"
		} else if p.Team == 2 {
			team = "allies"
		}
		out = append(out, map[string]any{
			"slot":       p.Slot,
			"name":       p.Name,
			"name_clean": cleanName(p.Name),
			"team":       team,
			"ping":       ping,
			"bot":        hasPing && ping == 0,
			"current":    p.Slot == curSlot,
		})
	}
	writeJSON(w, 200, map[string]any{"players": out, "server": server})
}

func handleFollow(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	var req struct {
		Slot *int `json:"slot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Slot == nil {
		writeJSON(w, 400, map[string]string{"error": `body must be {"slot":N}`})
		return
	}
	st.mu.Lock()
	ok := st.pipeExecLocked(fmt.Sprintf("follow %d", *req.Slot))
	if ok {
		st.curTargetSlot = *req.Slot
		// hold this pick for one director cycle; also protects it from the
		// free-cam healer during that window (player death/respawn)
		hold := time.Now().Add(time.Duration(cfg.DirMinSec) * time.Second)
		st.nextSwitch = hold
		st.manualUntil = hold
		st.freeSince = time.Time{}
	}
	st.mu.Unlock()
	if !ok {
		writeJSON(w, 503, map[string]string{"error": "pipeline not connected"})
		return
	}
	log.Printf("manual follow slot %d (via panel)", *req.Slot)
	writeJSON(w, 200, map[string]any{"ok": true, "slot": *req.Slot})
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	var since int64
	if s := r.URL.Query().Get("since"); s != "" {
		ok := true
		var n int64
		for _, c := range s {
			if c < '0' || c > '9' {
				ok = false
				break
			}
			n = n*10 + int64(c-'0')
		}
		if ok {
			since = n
		}
	}
	evs, hl, seg, scans, cur := feed.snapshot(since)
	writeJSON(w, 200, map[string]any{
		"seq":        cur,
		"events":     evs,
		"highlights": hl,
		"segments":   seg,
		"scans":      scans,
	})
}

func main() {
	loadConfig()
	if cfg.LogFile != "" {
		if f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			log.SetOutput(io.MultiWriter(os.Stdout, f))
		} else {
			log.Println("log: cannot open", cfg.LogFile, err)
		}
	}
	st.pipeAddr = cfg.PipeAddr // the live instance uses the existing pipe_addr
	go st.pipeLoop()
	go directorLoop()
	go watchdog()
	go audioMonitor()
	go winAudioMonitor()
	startSysMonitor(cfg.DiskPath)
	setupReplay() // second (replay) instance + goroutines; no-op unless enabled
	http.HandleFunc("/start", handleStart)
	http.HandleFunc("/switch", handleSwitch)
	http.HandleFunc("/exec", handleExec)
	http.HandleFunc("/stop", handleStop)
	http.HandleFunc("/status", handleStatus)
	http.HandleFunc("/events", handleEvents)
	http.HandleFunc("/players", handlePlayers)
	http.HandleFunc("/scene", handleScene)
	http.HandleFunc("/follow", handleFollow)
	http.HandleFunc("/log", handleLog)
	http.HandleFunc("/system", handleSystem)
	http.HandleFunc("/replay", handleReplay)
	http.HandleFunc("/replay/segments", handleReplaySegments)
	http.HandleFunc("/replay/stop", handleReplayStop)
	http.HandleFunc("/replay/status", handleReplayStatus)
	log.Println("wolffiles-stream-agent v3 listening on", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, nil))
}
