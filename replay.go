package main

// Replay playback: a second, non-streaming ET instance plays back a recorded
// demo segment around a highlight while the LIVE instance keeps streaming.
//
// SAFETY MODEL (non-negotiable): the replay path shares no mutable state with
// the live instance. Every failure mode -- instance won't start, demo missing,
// pipeline dead, OBS switch fails, caller aborts -- ends in the same place:
// log it, stop touching the replay, and make sure OBS is back on the LIVE
// scene. The live instance is never signalled from here.
//
// SEEK METHOD: the WTV status protocol does NOT carry the client's server time
// (see cl_wtvcontrol.c WTV_BuildStatusLine -- state/server/map/following/fps/
// percent only), so we cannot observe how far a demo has played from telemetry.
// Instead we fast-forward with `timescale <seek>` and estimate elapsed demo
// time as wall_time * timescale, dropping back to slow-mo shortly before the
// window. This intentionally biases toward UNDERSHOOT: at a high timescale the
// client often cannot render fast enough, so real demo time advances slower
// than wall*timescale -- meaning we arrive a little EARLY (safe: a bit more
// lead-in) rather than overshooting past the highlight. A safety margin
// (seekMarginMs) widens that bias.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// seekMarginMs is how much demo-time we stop the fast-forward SHORT of the
	// window start, to absorb wall-clock estimation error and bias to undershoot.
	seekMarginMs = 1500
	// replayStartTimeout bounds how long we wait for a freshly spawned replay
	// instance to open its pipeline and say hello. ET startup is slow.
	replayStartTimeout = 90 * time.Second
	// replayActiveTimeout bounds how long we wait for `demo` to reach playback.
	replayActiveTimeout = 25 * time.Second
	// replayStartupEtaSec is the rough cost of a cold instance start, for eta.
	replayStartupEtaSec = 15
)

// replayPort is the replay instance's cl_wtvPort, derived from replay_pipe_addr.
var replayPort int

/* ------------------------- pure, unit-tested helpers ------------------------ */

// offsetFromSvtime converts an absolute server time to an offset (ms) into a
// segment that started at segStart.
func offsetFromSvtime(svtime, segStart int) int { return svtime - segStart }

// svtimeInSegment reports whether an svtime falls inside a segment. An open
// segment (EndSv == 0, still recording) has no upper bound.
func svtimeInSegment(svtime int, seg DemoSegment) bool {
	if svtime < seg.StartSv {
		return false
	}
	return seg.EndSv == 0 || svtime <= seg.EndSv
}

// playbackWindow returns the [start,end] demo offsets (ms) to show: preMs
// before the highlight to postMs after, clamped so start is never negative.
func playbackWindow(offsetMs, preMs, postMs int) (startMs, endMs int) {
	startMs = offsetMs - preMs
	if startMs < 0 {
		startMs = 0
	}
	endMs = offsetMs + postMs
	if endMs < startMs {
		endMs = startMs
	}
	return startMs, endMs
}

// fastForwardUntilMs returns the demo offset (ms) at which to leave fast-forward:
// a safety margin before the window start, clamped at zero.
func fastForwardUntilMs(windowStartMs, marginMs int) int {
	t := windowStartMs - marginMs
	if t < 0 {
		t = 0
	}
	return t
}

// wallMsForDemoMs returns how much wall-clock time (ms) it takes to advance
// demoMs of demo time at the given timescale (demo time = wall * timescale).
func wallMsForDemoMs(demoMs int, timescale float64) int {
	if timescale <= 0 {
		timescale = 1
	}
	if demoMs < 0 {
		demoMs = 0
	}
	return int(float64(demoMs) / timescale)
}

// etaSeconds estimates how long a replay will take from acceptance to the end
// of the window: (cold start) + fast-forward + slow-mo window playback.
func etaSeconds(instanceUp bool, ffTargetMs, seekTS, windowMs int, speed float64) int {
	sec := 0.0
	if !instanceUp {
		sec += replayStartupEtaSec
	}
	sec += float64(wallMsForDemoMs(ffTargetMs, float64(seekTS))) / 1000.0
	sec += float64(wallMsForDemoMs(windowMs, speed)) / 1000.0
	return int(sec + 0.999) // ceil
}

// portFromAddr extracts the numeric port from a "host:port" address.
func portFromAddr(addr string) (int, bool) {
	i := strings.LastIndex(addr, ":")
	if i < 0 || i+1 >= len(addr) {
		return 0, false
	}
	n := 0
	for _, c := range addr[i+1:] {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// demoRelPath is the game-relative path used to locate a segment's demo file.
// Prefers the path the engine reported; falls back to <demoDir>/<file>.
func demoRelPath(seg DemoSegment, demoDir string) string {
	if seg.Path != "" {
		return seg.Path
	}
	return demoDir + "/" + seg.File
}

// demoAbsPath builds the absolute on-disk path of a demo recorded by the LIVE
// instance: <liveHome>/<fsGame>/<relPath>. Requires both to be known.
// CL_PlayDemo_f opens an absolute path (with .dm_84 extension) directly via
// FS_FOpenFileReadFullDir, so the replay instance -- which runs under a
// different fs_homepath -- can still load it with no copy.
func demoAbsPath(liveHome, fsGame, relPath string) (string, error) {
	if liveHome == "" || fsGame == "" {
		return "", errStr("live homepath / fs_game unknown -- set live_homepath and fs_game (or +set them in et_args) so the replay instance can locate demos")
	}
	p := filepath.Join(liveHome, fsGame, filepath.FromSlash(relPath))
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// demoLoadCommand is the console command that loads a demo by absolute path.
// The path is quoted so spaces survive the console tokenizer; the tokenizer
// strips the quotes before Sys_PathAbsolute sees the C:\ prefix.
func demoLoadCommand(absPath string) string {
	return `demo "` + absPath + `"`
}

// buildReplayArgs derives the replay instance's ET args from the live args:
// same video/config, but its own fs_homepath, cl_wtvPort and window title, and
// demo recording forced OFF (the replay instance must never record). Any
// +connect from the live args is dropped -- the replay instance joins nothing.
func buildReplayArgs(base []string, homepath, title string, port int) []string {
	out := stripArg(base, "+connect")          // never auto-connect
	out = setArg(out, "fs_homepath", homepath) // isolated homepath
	out = setArg(out, "cl_wtvPort", fmt.Sprintf("%d", port))
	out = setArg(out, "cl_wtvTitle", title)
	out = setArg(out, "cl_wtvDemo", "0")   // never record on the replay instance
	out = setArg(out, "cl_wtvFallback", "0") // no fallback director on replay
	return out
}

// setArg sets `+set <key> <val>` in an ET arg list, replacing an existing value
// (whether written as `+set key val` or a bare `key val`) or appending it.
func setArg(args []string, key, val string) []string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "+set" && i+2 < len(args) && args[i+1] == key {
			args[i+2] = val
			return args
		}
		if args[i] == key && i > 0 && args[i-1] == "+set" {
			args[i+1] = val
			return args
		}
	}
	return append(args, "+set", key, val)
}

// stripArg removes `flag <value>` (e.g. `+connect 1.2.3.4`) from an arg list.
func stripArg(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++ // skip the value too
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// highlightLabel renders a short human label for a highlight.
func highlightLabel(h Highlight) string {
	who := h.Player
	switch h.Kind {
	case "multikill":
		name := "Multikill"
		switch h.Count {
		case 2:
			name = "Double kill"
		case 3:
			name = "Triple kill"
		case 4:
			name = "Quad kill"
		default:
			if h.Count >= 5 {
				name = fmt.Sprintf("%d-kill", h.Count)
			}
		}
		if who != "" {
			return name + " by " + who
		}
		return name
	case "dynamite":
		return "Dynamite"
	case "objective":
		if who != "" {
			return "Objective by " + who
		}
		return "Objective"
	}
	if who != "" {
		return titleCase(h.Kind) + " by " + who
	}
	return titleCase(h.Kind)
}

// titleCase upper-cases the first byte of an ASCII word (avoids the deprecated
// strings.Title for our simple, ASCII-only highlight kinds).
func titleCase(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-32) + s[1:]
	}
	return s
}

type highlightView struct {
	Kind     string `json:"kind"`
	Player   string `json:"player"`
	Score    int    `json:"score"`
	SvTime   int    `json:"svtime"`
	OffsetMs int    `json:"offset_ms"`
	Label    string `json:"label"`
}

type segmentView struct {
	File       string          `json:"file"`
	Map        string          `json:"map"`
	StartSv    int             `json:"seg_start_svtime"`
	EndSv      int             `json:"seg_end_svtime"`
	Highlights []highlightView `json:"highlights"`
}

// segmentViews assembles the /replay/segments response: each segment with the
// highlights that fall inside its svtime range, newest segment first.
func segmentViews(segs []DemoSegment, hls []Highlight) []segmentView {
	out := make([]segmentView, 0, len(segs))
	for i := len(segs) - 1; i >= 0; i-- { // newest first
		seg := segs[i]
		sv := segmentView{
			File: seg.File, Map: seg.Map,
			StartSv: seg.StartSv, EndSv: seg.EndSv,
			Highlights: []highlightView{},
		}
		for _, h := range hls {
			if !svtimeInSegment(h.SvTime, seg) {
				continue
			}
			sv.Highlights = append(sv.Highlights, highlightView{
				Kind: h.Kind, Player: h.Player, Score: h.Score,
				SvTime:   h.SvTime,
				OffsetMs: offsetFromSvtime(h.SvTime, seg.StartSv),
				Label:    highlightLabel(h),
			})
		}
		out = append(out, sv)
	}
	return out
}

/* ------------------------------ controller ---------------------------------- */

// replayJob is the immutable description of one replay run.
type replayJob struct {
	file     string
	absPath  string
	offsetMs int
	preMs    int
	postMs   int
	speed    float64
}

type replayController struct {
	mu        sync.Mutex
	active    bool
	phase     string // idle|starting|seeking|playing|returning
	file      string
	offsetMs  int
	startedAt int64
	cancel    chan struct{}
	lastDone  time.Time
}

var replay replayController

// begin atomically claims the single replay slot. Returns false if one is
// already running (the caller returns 409).
func (rc *replayController) begin(job replayJob) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.active {
		return false
	}
	rc.active = true
	rc.phase = "starting"
	rc.file = job.file
	rc.offsetMs = job.offsetMs
	rc.startedAt = time.Now().Unix()
	rc.cancel = make(chan struct{})
	return true
}

func (rc *replayController) setPhase(p string) {
	rc.mu.Lock()
	rc.phase = p
	rc.mu.Unlock()
	log.Printf("replay: phase -> %s", p)
}

// stop aborts a running replay (or, if idle, just re-asserts the live scene).
func (rc *replayController) stop() {
	rc.mu.Lock()
	if rc.active && rc.cancel != nil {
		select {
		case <-rc.cancel:
		default:
			close(rc.cancel)
		}
		rc.mu.Unlock()
		log.Println("replay: abort requested")
		return
	}
	rc.mu.Unlock()
	// idle: make sure OBS is on the live scene regardless.
	_ = rc.cutScene("live", cfg.DryRun)
}

func (rc *replayController) status() map[string]any {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	instUp := false
	if rp != nil {
		rp.mu.Lock()
		instUp = rp.etCmd != nil && rp.pipeUp
		rp.mu.Unlock()
	}
	phase := rc.phase
	if phase == "" {
		phase = "idle"
	}
	return map[string]any{
		"active":      rc.active,
		"phase":       phase,
		"file":        rc.file,
		"offset_ms":   rc.offsetMs,
		"started_at":  rc.startedAt,
		"instance_up": instUp,
	}
}

// replayExec sends a console command to the replay instance (logged; a no-op
// send in dry-run). Never touches the live instance.
func (rc *replayController) replayExec(line string, dry bool) bool {
	log.Printf("replay: exec %q", line)
	if dry {
		return true
	}
	if rp == nil {
		return false
	}
	rp.mu.Lock()
	ok := rp.pipeExecLocked(line)
	rp.mu.Unlock()
	return ok
}

// cutScene switches OBS to a logical scene (live/replay/standby). In dry-run it
// only logs. A missing scene name is an error the caller handles.
func (rc *replayController) cutScene(logical string, dry bool) error {
	name := resolveScene(logical)
	if name == "" {
		return errStr("scene not configured: " + logical)
	}
	log.Printf("replay: OBS scene -> %s (%s)", logical, name)
	if dry {
		return nil
	}
	return setScene(name)
}

// sleepAbortable waits d, returning false if the replay was aborted. In dry-run
// it collapses the wait so the whole sequence runs fast.
func (rc *replayController) sleepAbortable(d time.Duration, cancel <-chan struct{}, dry bool) bool {
	if dry {
		log.Printf("replay: [dry-run] would wait %v", d.Round(time.Millisecond))
		d = 20 * time.Millisecond
	}
	if d < 0 {
		d = 0
	}
	select {
	case <-time.After(d):
		return true
	case <-cancel:
		log.Println("replay: aborted during wait")
		return false
	}
}

// run executes the full replay sequence. It ALWAYS ends via finish(), which
// returns OBS to the live scene, so no early return can leave the broadcast on
// the replay scene.
func (rc *replayController) run(job replayJob) {
	dry := cfg.DryRun
	rc.mu.Lock()
	cancel := rc.cancel
	rc.mu.Unlock()
	defer rc.finish(dry)

	// 1) ensure the replay instance is up and the demo is playing.
	if !dry {
		if err := ensureReplayUp(replayStartTimeout, cancel); err != nil {
			log.Println("replay: instance not ready:", err)
			return
		}
	} else {
		log.Println("replay: [dry-run] would ensure replay instance is up")
	}

	if !rc.replayExec(demoLoadCommand(job.absPath), dry) {
		log.Println("replay: demo load failed (replay pipeline down)")
		return
	}
	if !dry {
		if !waitReplayActive(replayActiveTimeout, cancel) {
			log.Println("replay: demo did not reach playback (timeout/abort)")
			return
		}
	}

	// 2) seek: fast-forward to just before the window.
	rc.setPhase("seeking")
	windowStart, windowEnd := playbackWindow(job.offsetMs, job.preMs, job.postMs)
	ffTarget := fastForwardUntilMs(windowStart, seekMarginMs)
	seekTS := cfg.ReplaySeekTimescale
	rc.replayExec(fmt.Sprintf("timescale %d", seekTS), dry)
	ffWall := wallMsForDemoMs(ffTarget, float64(seekTS))
	if !rc.sleepAbortable(time.Duration(ffWall)*time.Millisecond, cancel, dry) {
		return
	}

	// 3) drop into slow-mo, THEN cut to the replay scene. Cutting only after a
	// successful timescale change means a failed OBS switch never leaves the
	// live scene showing a fast-forward.
	rc.setPhase("playing")
	rc.replayExec(fmt.Sprintf("timescale %.3f", job.speed), dry)
	if err := rc.cutScene("replay", dry); err != nil {
		log.Println("replay: OBS switch to replay failed -- staying on live, aborting:", err)
		return
	}

	// 4) play the window at slow speed, then finish() cuts back to live.
	windowMs := windowEnd - windowStart
	playWall := wallMsForDemoMs(windowMs, job.speed)
	rc.sleepAbortable(time.Duration(playWall)*time.Millisecond, cancel, dry)
}

// finish is the single exit point: reset timescale, ALWAYS return OBS to the
// live scene, and mark the controller idle. Runs even on panic/early-return.
func (rc *replayController) finish(dry bool) {
	rc.setPhase("returning")
	rc.replayExec("timescale 1", dry)
	if err := rc.cutScene("live", dry); err != nil {
		log.Println("replay: WARNING could not cut back to live scene:", err)
	}
	rc.mu.Lock()
	rc.active = false
	rc.phase = "idle"
	rc.file = ""
	rc.offsetMs = 0
	rc.cancel = nil
	rc.lastDone = time.Now()
	rc.mu.Unlock()
	log.Println("replay: done -- live scene restored")
}

/* ------------------------ replay instance lifecycle ------------------------- */

// ensureReplayUp starts the replay instance if needed and waits for its
// pipeline hello, honouring an abort. Never affects the live instance.
func ensureReplayUp(timeout time.Duration, cancel <-chan struct{}) error {
	if rp == nil {
		return errStr("replay instance not configured")
	}
	rp.mu.Lock()
	if rp.etCmd == nil {
		args := buildReplayArgs(cfg.EtArgs, cfg.ReplayHomepath, cfg.ReplayTitle, replayPort)
		log.Printf("replay: starting instance: %s %s", cfg.EtPath, strings.Join(args, " "))
		if err := rp.spawn(args); err != nil {
			rp.mu.Unlock()
			return err
		}
	}
	rp.mu.Unlock()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-cancel:
			return errStr("aborted while waiting for replay instance")
		default:
		}
		rp.mu.Lock()
		up := rp.pipeUp && rp.pipeCaps != nil
		rp.mu.Unlock()
		if up {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errStr("replay pipeline hello timeout")
}

// waitReplayActive waits until the replay instance reports demo playback is
// active (CA_ACTIVE), or the timeout/abort fires.
func waitReplayActive(timeout time.Duration, cancel <-chan struct{}) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-cancel:
			return false
		default:
		}
		rp.mu.Lock()
		active := rp.tele.State == "active"
		rp.mu.Unlock()
		if active {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// replayIdleMonitor stops the warm replay instance after it has been idle for
// replay_idle_stop_sec, so back-to-back replays stay fast but an idle instance
// does not linger forever.
func replayIdleMonitor() {
	for {
		time.Sleep(10 * time.Second)
		if rp == nil {
			continue
		}
		// Decide AND stop atomically under replay.mu so a POST /replay cannot
		// begin() (and start using the instance) between the idle check and the
		// kill. Lock order is always replay.mu -> rp.mu (same as status()).
		replay.mu.Lock()
		idle := !replay.active && !replay.lastDone.IsZero() &&
			time.Since(replay.lastDone) >= time.Duration(cfg.ReplayIdleStopSec)*time.Second
		if idle {
			rp.mu.Lock()
			if rp.etCmd != nil {
				log.Printf("replay: instance idle %ds -> stopping", cfg.ReplayIdleStopSec)
				rp.kill()
			}
			rp.mu.Unlock()
			replay.lastDone = time.Time{} // disarm; nothing to stop until the next replay
		}
		replay.mu.Unlock()
	}
}

/* --------------------------------- HTTP ------------------------------------- */

// GET /replay/segments -> segments (newest first) with in-range highlights.
func handleReplaySegments(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	views := segmentViews(feed.segmentsSnapshot(), feed.highlightsSnapshot())
	writeJSON(w, 200, map[string]any{"segments": views})
}

type replayReq struct {
	File     string   `json:"file"`
	SvTime   *int     `json:"svtime"`
	OffsetMs *int     `json:"offset_ms"`
	PreSec   *int     `json:"pre_sec"`
	PostSec  *int     `json:"post_sec"`
	Speed    *float64 `json:"speed"`
}

// POST /replay -> start a replay. 409 if one is running, 400 on bad input.
func handleReplay(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	if !cfg.ReplayEnabled || rp == nil {
		writeJSON(w, 400, map[string]string{"error": "replay disabled (set replay_enabled)"})
		return
	}
	var req replayReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.File == "" {
		writeJSON(w, 400, map[string]string{"error": `body must be {"file":"...","svtime":N} or {"file":"...","offset_ms":N}`})
		return
	}

	seg, ok := feed.findSegment(req.File)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "unknown demo file: " + req.File})
		return
	}

	// resolve the offset from svtime or an explicit offset_ms
	var offsetMs int
	switch {
	case req.SvTime != nil:
		offsetMs = offsetFromSvtime(*req.SvTime, seg.StartSv)
	case req.OffsetMs != nil:
		offsetMs = *req.OffsetMs
	default:
		writeJSON(w, 400, map[string]string{"error": "provide svtime or offset_ms"})
		return
	}
	segLen := seg.EndSv - seg.StartSv
	if offsetMs < 0 || (seg.EndSv > 0 && offsetMs > segLen) {
		writeJSON(w, 400, map[string]string{"error": "offset outside the segment"})
		return
	}

	// locate the demo file on disk (absolute path; read directly by the client)
	absPath, err := demoAbsPath(cfg.LiveHomepath, cfg.FsGame, demoRelPath(seg, cfg.ReplayDemoDir))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !cfg.DryRun {
		if _, serr := os.Stat(absPath); serr != nil {
			writeJSON(w, 400, map[string]string{"error": "demo file not on disk (rotated out?): " + absPath})
			return
		}
	}

	pre := cfg.ReplayPreSec
	post := cfg.ReplayPostSec
	speed := cfg.ReplaySpeed
	if req.PreSec != nil && *req.PreSec >= 0 {
		pre = *req.PreSec
	}
	if req.PostSec != nil && *req.PostSec >= 0 {
		post = *req.PostSec
	}
	if req.Speed != nil && *req.Speed > 0 {
		speed = *req.Speed
	}

	job := replayJob{
		file: seg.File, absPath: absPath, offsetMs: offsetMs,
		preMs: pre * 1000, postMs: post * 1000, speed: speed,
	}
	if !replay.begin(job) {
		writeJSON(w, 409, map[string]string{"error": "a replay is already running"})
		return
	}

	// eta based on current instance state and the seek/window math
	rp.mu.Lock()
	instUp := rp.etCmd != nil && rp.pipeUp
	rp.mu.Unlock()
	windowStart, windowEnd := playbackWindow(offsetMs, job.preMs, job.postMs)
	ffTarget := fastForwardUntilMs(windowStart, seekMarginMs)
	eta := etaSeconds(instUp, ffTarget, cfg.ReplaySeekTimescale, windowEnd-windowStart, speed)

	go replay.run(job)

	log.Printf("replay: accepted %s offset %dms (pre %ds post %ds speed %.2f) eta %ds",
		seg.File, offsetMs, pre, post, speed, eta)
	writeJSON(w, 200, map[string]any{
		"ok": true, "file": seg.File, "offset_ms": offsetMs, "eta_sec": eta,
	})
}

// POST /replay/stop -> abort and cut back to live immediately.
func handleReplayStop(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	replay.stop()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /replay/status -> current replay state.
func handleReplayStatus(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	writeJSON(w, 200, replay.status())
}

// setupReplay wires the replay instance and its background goroutines. Called
// from main() when replay_enabled. Leaves the live instance untouched.
func setupReplay() {
	if !cfg.ReplayEnabled {
		log.Println("replay: disabled (replay_enabled false)")
		return
	}
	port, ok := portFromAddr(cfg.ReplayPipeAddr)
	if !ok {
		log.Println("replay: DISABLED -- invalid replay_pipe_addr:", cfg.ReplayPipeAddr)
		cfg.ReplayEnabled = false
		return
	}
	if cfg.ReplayHomepath == "" {
		log.Println("replay: DISABLED -- replay_homepath is required")
		cfg.ReplayEnabled = false
		return
	}
	replayPort = port
	rp = &instance{
		name:       "replay",
		homepath:   cfg.ReplayHomepath,
		pipeAddr:   cfg.ReplayPipeAddr,
		directs:    false, // the replay orchestrator drives the camera, not the director
		feedEvents: false, // replayed demos must NOT pollute the live event feed
	}
	go rp.pipeLoop()
	go replayIdleMonitor()
	log.Printf("replay: enabled -- instance pipe %s, homepath %s, dry_run %v",
		cfg.ReplayPipeAddr, cfg.ReplayHomepath, cfg.DryRun)
}
