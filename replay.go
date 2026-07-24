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

/* ------------------------------ mod resolution ------------------------------ */

// A demo can only be played back by the mod that recorded it: the demo stream
// carries that mod's gamestate, and the client checks the pk3s it references.
// The client stores each segment under a mod directory inside the flat demo
// root and reports both parts on the pipeline:
//
//	"file" = "wtv_<map>_<svtime>.dm_84"        (basename)
//	"path" = "<mod>/wtv_<map>_<svtime>.dm_84"  (relative to the demo root)
//	"mod"  = "<mod>"
//
// so the mod comes from the message, either as its own field or as the
// directory in "path". Nothing is parsed out of the filename: a mod name
// (no_quarter) and a map name (etl_sp_delivery) can both contain underscores,
// which is exactly why the client puts the mod in a path segment.

// validDemoPath mirrors the client's WTV_ValidDemoRelPath: the argument of
// `wtvdemo` is either "<mod>/<file>" -- exactly one directory level -- or a
// bare "<file>", with each segment made only of [A-Za-z0-9._-] and never "."
// or "..". The value reaches us over the pipeline and leaves as a console
// line, so it is validated on this side too rather than trusted.
func validDemoPath(p string) bool {
	if p == "" || len(p) > 128 || strings.Contains(p, `\`) {
		return false
	}
	mod, file := splitDemoPath(p)
	if mod == "" && strings.Contains(p, "/") {
		return false // leading slash, or more than one directory level
	}
	if mod != "" && !validPathSegment(mod) {
		return false
	}
	return validPathSegment(file)
}

// validPathSegment is one component of a demo path: non-empty, [A-Za-z0-9._-]
// only, never "." or "..".
func validPathSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// splitDemoPath splits "<mod>/<file>" into its parts. A bare filename yields an
// empty mod. More than one separator yields an empty mod and the whole string
// as the file, which validDemoPath then rejects.
func splitDemoPath(p string) (mod, file string) {
	i := strings.Index(p, "/")
	if i < 0 || strings.Contains(p[i+1:], "/") {
		return "", p
	}
	return p[:i], p[i+1:]
}

// resolveSegmentMod returns the mod that recorded a segment and where that
// answer came from: "field" = the client reported it outright, "directory" =
// taken from the mod directory of the segment's demo-root-relative path.
func resolveSegmentMod(seg DemoSegment) (mod, source string, err error) {
	if seg.Mod != "" {
		return seg.Mod, "field", nil
	}
	if m, _ := splitDemoPath(seg.Path); m != "" && validPathSegment(m) {
		return m, "directory", nil
	}
	return "", "", errStr("cannot determine which mod recorded " + seg.File +
		" -- the client reported neither a mod field nor a <mod>/<file> path" +
		" (demo predates the per-mod demo directory); it cannot be replayed")
}

/* --------------------------- demo file addressing --------------------------- */

// demoPlayArg is the string handed to the client's `wtvdemo` command: the
// segment's path relative to the demo root, which already carries the mod
// directory. Falls back to the bare basename, which `wtvdemo` also accepts,
// when the client did not report a path.
func demoPlayArg(seg DemoSegment) string {
	if seg.Path != "" {
		return seg.Path
	}
	return seg.File
}

// demoAbsPath is the on-disk path of a demo, used only to check that the file
// is still there before starting a replay. The client writes demos into one
// root under fs_homepath (cl_wtvDemoPath) with one sub-directory per mod, and
// both instances share that homepath:
//
//	<live_homepath>/<replay_demo_dir>/<mod>/<file>
//
// relPath is that "<mod>/<file>" part. Its separator is a forward slash (it
// comes from the engine), so it is converted before being joined -- see
// TestDemoAbsPathOnDisk, which checks the result really stats on this platform.
func demoAbsPath(liveHome, demoDir, relPath string) (string, error) {
	if liveHome == "" {
		return "", errStr("live homepath unknown -- set live_homepath (or +set fs_homepath in et_args) so the agent can find demos")
	}
	if demoDir == "" {
		demoDir = "wtvdemos"
	}
	abs, err := filepath.Abs(filepath.Join(liveHome,
		filepath.FromSlash(demoDir), filepath.FromSlash(relPath)))
	if err != nil {
		return "", err
	}
	return abs, nil
}

// demoLoadCommand is the console command that loads a demo on the replay
// instance. `wtvdemo` reads from the flat demo root, bypassing the
// fs_game-relative lookup that plain `demo` does.
func demoLoadCommand(relPath string) string {
	return "wtvdemo " + relPath
}

/* ---------------------------- replay instance args -------------------------- */

// replayForcedCvars are the cvars the replay instance overrides on its own
// command line. They are also the ones it can leak into the SHARED config file
// (see sharedHomepathRisks) if the live instance does not set them itself.
var replayForcedCvars = []string{"cl_wtvDemo", "cl_wtvFallback", "s_initsound", "db_mode"}

// buildReplayArgs derives the replay instance's ET args from the live args.
//
// The replay instance runs under the SAME fs_homepath as the live instance --
// that is not a convenience, it is a requirement: a demo only plays if the
// client can see the same pk3s (maps + mod) the live instance downloaded, and
// those live in the live homepath. An empty separate homepath fails playback
// with a checksum error. The two are separated by ET profile instead.
//
// fs_game is the MOD THAT RECORDED THE DEMO, not the live one.
func buildReplayArgs(base []string, homepath, mod, profile, title string, port int) []string {
	out := stripArg(base, "+connect") // never auto-connect
	out = setArg(out, "fs_homepath", homepath)
	out = setArg(out, "fs_game", mod) // demos are only playable by their own mod
	out = setArg(out, "cl_wtvPort", fmt.Sprintf("%d", port))
	out = setArg(out, "cl_wtvTitle", title)
	out = setArg(out, "cl_wtvDemo", "0")     // never record on the replay instance
	out = setArg(out, "cl_wtvFallback", "0") // no fallback director on replay
	// Sharing the homepath means sharing files. Keep the replay instance from
	// touching the ones the live instance owns:
	out = setArg(out, "s_initsound", "0") // no second process fighting for the audio device
	out = setArg(out, "db_mode", "1")     // in-memory DB, do not open the shared etl.db
	out = setArg(out, "logfile", "0")     // etconsole.log is opened truncating -- would eat live's log
	if profile != "" {
		// cl_profile moves the startup config exec and the profile.pid out of
		// the way; com_pidfile is set explicitly because cl_profile is CVAR_ROM
		// and gets reset to "" once CL_Init registers it (see README).
		out = setArg(out, "cl_profile", profile)
		out = setArg(out, "com_pidfile", "profiles/"+profile+"/profile.pid")
	}
	return out
}

// sharedHomepathRisks lists the cvars the replay instance forces that the live
// instance does NOT pin in et_args. Both instances write the same
// <fs_homepath>/<fs_game>/etconfig.cfg (ET writes the config from Com_Frame
// whenever an archived cvar changes, and cl_profile is empty at that point), so
// a value forced here can end up in the file the live instance reads on its
// next start. Pinning it in et_args makes the live instance immune: command
// line +set is applied AFTER the config is exec'd.
func sharedHomepathRisks(etArgs []string) []string {
	var out []string
	for _, k := range replayForcedCvars {
		if argValue(etArgs, k) == "" {
			out = append(out, k)
		}
	}
	return out
}

/* ------------------------ warm-instance mod decision ------------------------ */

type instanceAction int

const (
	instReuse   instanceAction = iota // warm instance already runs the right mod
	instStart                         // nothing running -- cold start
	instRestart                       // running the wrong mod -- must be restarted
)

func (a instanceAction) String() string {
	switch a {
	case instReuse:
		return "reuse"
	case instStart:
		return "start"
	default:
		return "restart"
	}
}

// replayInstanceAction decides what to do with the warm replay instance for a
// demo recorded by wantMod. A warm instance is NEVER reused across mods: its
// fs_game is fixed at launch, and loading a foreign demo would restart the
// filesystem mid-playback (or simply fail to find the mod's pk3s).
func replayInstanceAction(running bool, curMod, wantMod string) instanceAction {
	if !running {
		return instStart
	}
	if !strings.EqualFold(curMod, wantMod) {
		return instRestart
	}
	return instReuse
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
	// Camera context (Part 1). Replayable is false when the live camera was not
	// on the subject: the panel greys out the Play button for those, and the
	// auto-director never picks them.
	Replayable   bool   `json:"replayable"`
	Followed     string `json:"followed,omitempty"`
	FollowedSlot int    `json:"followed_slot"`
}

type segmentView struct {
	File string `json:"file"`
	// Path is the demo-root-relative "<mod>/<file>" the client reported, i.e.
	// what `wtvdemo` is given verbatim.
	Path string `json:"path,omitempty"`
	Map  string `json:"map"`
	// Mod is the fs_game that recorded the demo -- the replay instance must run
	// it to play the demo back. "" means it could not be determined and the
	// segment is not replayable (mod_source says why).
	Mod        string          `json:"mod"`
	ModSource  string          `json:"mod_source"` // field|directory|unknown
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
		mod, source, err := resolveSegmentMod(seg)
		if err != nil {
			source = "unknown"
		}
		sv := segmentView{
			File: seg.File, Path: seg.Path, Map: seg.Map,
			Mod: mod, ModSource: source,
			StartSv: seg.StartSv, EndSv: seg.EndSv,
			Highlights: []highlightView{},
		}
		for _, h := range hls {
			if !svtimeInSegment(h.SvTime, seg) {
				continue
			}
			sv.Highlights = append(sv.Highlights, highlightView{
				Kind: h.Kind, Player: h.Player, Score: h.Score,
				SvTime:       h.SvTime,
				OffsetMs:     offsetFromSvtime(h.SvTime, seg.StartSv),
				Label:        highlightLabel(h),
				Replayable:   h.Replayable,
				Followed:     h.Followed,
				FollowedSlot: h.FollowedSlot,
			})
		}
		out = append(out, sv)
	}
	return out
}

/* ------------------------------ controller ---------------------------------- */

// replayJob is the immutable description of one replay run.
type replayJob struct {
	file     string // basename, the identifier the panel and /events use
	path     string // "<mod>/<file>" relative to the demo root, for `wtvdemo`
	mod      string // fs_game that recorded it; the replay instance must run it
	absPath  string // on-disk location, for the existence check and logs
	offsetMs int
	preMs    int
	postMs   int
	speed    float64
}

type replayController struct {
	mu        sync.Mutex
	active    bool
	phase     string // idle|starting|loading|seeking|prepared|playing|returning
	file      string
	mod       string
	offsetMs  int
	startedAt int64
	cancel    chan struct{}
	lastDone  time.Time // when the slot was last released (any teardown) -- idle-stop timer
	lastAired time.Time // when a replay last actually AIRED -- the min-interval spacing
	// auto-director preparation (Part 3): the warm instance is loaded, seeked
	// and held so that when the lull comes only the OBS cut + playback remain.
	// auto marks the current job as director-driven (vs a manual /replay), so the
	// director may tear it down (map change, auto disabled) without touching a
	// manual replay.
	auto         bool
	autoPrepared bool
	preparedAt   time.Time
	preparedJob  replayJob
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
	rc.mod = job.mod
	rc.offsetMs = job.offsetMs
	rc.startedAt = time.Now().Unix()
	rc.cancel = make(chan struct{})
	rc.auto = false
	rc.autoPrepared = false
	rc.preparedJob = job
	return true
}

// isActive reports whether the single replay slot is claimed (preparing,
// holding, or playing).
func (rc *replayController) isActive() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.active
}

// isPrepared reports whether a replay is loaded, seeked and holding, ready for
// an instant cut.
func (rc *replayController) isPrepared() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.active && rc.autoPrepared
}

// lastDoneTime returns when the last replay finished (zero if none yet).
func (rc *replayController) lastDoneTime() time.Time {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.lastDone
}

// preparedSince returns when the current preparation started holding (zero if
// not holding).
func (rc *replayController) preparedSince() time.Time {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.preparedAt
}

// preparedView describes any prepared/holding clip for /director/status.
func (rc *replayController) preparedView() map[string]any {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if !rc.active {
		return nil
	}
	v := map[string]any{
		"file":  rc.preparedJob.file,
		"mod":   rc.preparedJob.mod,
		"phase": rc.phase,
		"held":  rc.autoPrepared,
	}
	if !rc.preparedAt.IsZero() {
		v["holding_for_sec"] = int(time.Since(rc.preparedAt).Seconds())
	}
	return v
}

func (rc *replayController) setPhase(p string) {
	rc.mu.Lock()
	rc.phase = p
	rc.mu.Unlock()
	log.Printf("replay: phase -> %s", p)
}

// stop aborts a running replay (or, if idle, just re-asserts the live scene).
// A HELD preparation has no goroutine waiting on cancel, so closing cancel would
// do nothing and leak the slot -- that case is torn down directly via finish().
func (rc *replayController) stop() {
	rc.mu.Lock()
	held := rc.active && rc.autoPrepared
	running := rc.active && !rc.autoPrepared && rc.cancel != nil
	rc.mu.Unlock()
	switch {
	case running:
		// a goroutine (run/runPrepare/runTrigger) is waiting on cancel
		rc.mu.Lock()
		if rc.cancel != nil {
			select {
			case <-rc.cancel:
			default:
				close(rc.cancel)
			}
		}
		rc.mu.Unlock()
		log.Println("replay: abort requested")
	case held:
		log.Println("replay: abort -- tearing down held preparation")
		rc.finish(cfg.DryRun, false) // never aired -> do not consume the interval
	default:
		// idle: make sure OBS is on the live scene regardless.
		_ = rc.cutScene("live", cfg.DryRun)
	}
}

// lastAiredTime returns when a replay last actually aired (zero if none yet);
// this, not lastDone, gates the minimum spacing between replays.
func (rc *replayController) lastAiredTime() time.Time {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.lastAired
}

func (rc *replayController) status() map[string]any {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	instUp := false
	instMod := ""
	if rp != nil {
		rp.mu.Lock()
		instUp = rp.etCmd != nil && rp.pipeUp
		instMod = rp.fsGame
		rp.mu.Unlock()
	}
	phase := rc.phase
	if phase == "" {
		phase = "idle"
	}
	return map[string]any{
		"active":       rc.active,
		"phase":        phase,
		"file":         rc.file,
		"mod":          rc.mod,
		"offset_ms":    rc.offsetMs,
		"started_at":   rc.startedAt,
		"instance_up":  instUp,
		"instance_mod": instMod,
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
	defer rc.finish(dry, true) // a manual replay airs (or aborts mid-air)

	// 1) ensure the replay instance is up, running the mod that recorded this
	//    demo, and that the demo is playing.
	if !dry {
		if err := ensureReplayUp(job.mod, replayStartTimeout, cancel); err != nil {
			log.Println("replay: instance not ready:", err)
			return
		}
	} else {
		log.Printf("replay: [dry-run] demo %q (mod %q, on disk %s) -- would %s the replay instance",
			job.path, job.mod, job.absPath, dryRunInstanceAction(job.mod))
	}

	if !rc.replayExec(demoLoadCommand(job.path), dry) {
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
	if !rc.seekTo(fastForwardUntilMs(windowStart, seekMarginMs), cancel, dry) {
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
// aired=true means a replay actually reached the broadcast (so it counts toward
// the min-interval spacing); a failed/aborted preparation passes false so a
// clip that never showed does not impose the full spacing on the next attempt.
func (rc *replayController) finish(dry bool, aired bool) {
	rc.setPhase("returning")
	rc.replayExec("timescale 1", dry)
	if err := rc.cutScene("live", dry); err != nil {
		log.Println("replay: WARNING could not cut back to live scene:", err)
	}
	rc.mu.Lock()
	rc.active = false
	rc.auto = false
	rc.phase = "idle"
	rc.file = ""
	rc.mod = ""
	rc.offsetMs = 0
	rc.cancel = nil
	rc.autoPrepared = false
	rc.preparedJob = replayJob{}
	rc.preparedAt = time.Time{}
	rc.lastDone = time.Now()
	if aired {
		rc.lastAired = time.Now()
	}
	rc.mu.Unlock()
	log.Println("replay: done -- live scene restored")
}

// seekTo fast-forwards the replay demo to targetMs of demo time. Two methods:
// "fastforward" uses the client's parse-level fastforward command (near-instant,
// accurate: it advances server time directly), "timescale" (default, the
// known-working manual path) plays at a high timescale for a wall-clock-estimated
// duration and biases to undershoot. Returns false if aborted mid-seek.
func (rc *replayController) seekTo(targetMs int, cancel <-chan struct{}, dry bool) bool {
	if targetMs <= 0 {
		return true
	}
	if cfg.ReplaySeekMode == "fastforward" {
		rc.replayExec(fmt.Sprintf("fastforward %.3f", float64(targetMs)/1000.0), dry)
		return rc.sleepAbortable(500*time.Millisecond, cancel, dry) // let the parse settle
	}
	seekTS := cfg.ReplaySeekTimescale
	rc.replayExec(fmt.Sprintf("timescale %d", seekTS), dry)
	ffWall := wallMsForDemoMs(targetMs, float64(seekTS))
	return rc.sleepAbortable(time.Duration(ffWall)*time.Millisecond, cancel, dry)
}

/* --------------------- auto-director prepare / trigger (Part 3) -------------- */

// prepareAuto claims the replay slot and, in a goroutine, gets the replay
// instance to the point where only the OBS cut + slow-mo playback remain: the
// instance is up on the right mod, the demo is loaded and playing, seeked to
// just before the window, and HELD at timescale 0. OBS stays on the live scene
// the entire time, so any failure here never shows on the broadcast.
//
// HOLD CAVEAT: the hold uses `timescale 0`. Whether ET truly freezes the demo
// parse at timescale 0 (vs. slowly drifting) could not be verified on the dev
// box -- see docs. If it drifts, the played window will start late; disable
// auto_replay and the known-working manual path (seek-and-play in one go) is
// unaffected. The seek method is cfg.ReplaySeekMode.
func (rc *replayController) prepareAuto(job replayJob) bool {
	if !rc.begin(job) {
		return false
	}
	rc.mu.Lock()
	rc.auto = true // director-driven -> discardPrepared may tear it down
	rc.mu.Unlock()
	go rc.runPrepare(job)
	return true
}

func (rc *replayController) runPrepare(job replayJob) {
	dry := cfg.DryRun
	rc.mu.Lock()
	cancel := rc.cancel
	rc.mu.Unlock()

	ok := func() bool {
		if !dry {
			if err := ensureReplayUp(job.mod, replayStartTimeout, cancel); err != nil {
				log.Println("replay: prepare -- instance not ready:", err)
				return false
			}
		} else {
			log.Printf("replay: [dry-run] prepare demo %q (mod %q) -- would %s the replay instance",
				job.path, job.mod, dryRunInstanceAction(job.mod))
		}
		rc.setPhase("loading")
		if !rc.replayExec(demoLoadCommand(job.path), dry) {
			log.Println("replay: prepare -- demo load failed (replay pipeline down)")
			return false
		}
		if !dry && !waitReplayActive(replayActiveTimeout, cancel) {
			log.Println("replay: prepare -- demo did not reach playback (timeout/abort)")
			return false
		}
		rc.setPhase("seeking")
		windowStart, _ := playbackWindow(job.offsetMs, job.preMs, job.postMs)
		if !rc.seekTo(fastForwardUntilMs(windowStart, seekMarginMs), cancel, dry) {
			return false
		}
		rc.replayExec("timescale 0", dry) // HOLD until the lull
		return true
	}()

	if !ok {
		rc.finish(dry, false) // never aired -> release slot, don't consume the interval
		return
	}
	rc.mu.Lock()
	rc.phase = "prepared"
	rc.autoPrepared = true
	rc.preparedAt = time.Now()
	rc.mu.Unlock()
	log.Printf("replay: prepared and holding %q -- waiting for a lull", job.file)
}

// triggerPrepared plays out a prepared replay: slow-mo, cut to the replay scene,
// play the window, then finish() returns to live. Returns false if nothing is
// prepared. Called when the auto-director detects a lull.
func (rc *replayController) triggerPrepared() bool {
	rc.mu.Lock()
	if !rc.active || !rc.autoPrepared {
		rc.mu.Unlock()
		return false
	}
	rc.autoPrepared = false
	job := rc.preparedJob
	cancel := rc.cancel
	rc.mu.Unlock()
	go rc.runTrigger(job, cancel)
	return true
}

func (rc *replayController) runTrigger(job replayJob, cancel <-chan struct{}) {
	dry := cfg.DryRun
	defer rc.finish(dry, true) // a triggered replay airs
	rc.setPhase("playing")
	rc.replayExec(fmt.Sprintf("timescale %.3f", job.speed), dry)
	if err := rc.cutScene("replay", dry); err != nil {
		log.Println("replay: OBS switch to replay failed -- staying on live, aborting:", err)
		return
	}
	windowStart, windowEnd := playbackWindow(job.offsetMs, job.preMs, job.postMs)
	playWall := wallMsForDemoMs(windowEnd-windowStart, job.speed)
	rc.sleepAbortable(time.Duration(playWall)*time.Millisecond, cancel, dry)
}

// discardPrepared tears down an auto-director preparation -- whether it is
// already holding or still in flight -- and returns to idle. A MANUAL replay is
// never touched. Used when the lull never came, the map changed, a better
// candidate appeared, or auto-replay was turned off. OBS is already on live.
func (rc *replayController) discardPrepared() {
	rc.mu.Lock()
	if !rc.active || !rc.auto {
		rc.mu.Unlock()
		return // idle, or a manual replay is running -> leave it alone
	}
	if rc.autoPrepared {
		// held: no goroutine is waiting on cancel, tear down directly.
		rc.mu.Unlock()
		log.Println("replay: discarding held preparation")
		rc.finish(cfg.DryRun, false)
		return
	}
	// in-flight prepare: a goroutine is in runPrepare waiting on cancel; closing
	// it makes runPrepare bail and call finish() itself.
	if rc.cancel != nil {
		select {
		case <-rc.cancel:
		default:
			close(rc.cancel)
		}
	}
	rc.mu.Unlock()
	log.Println("replay: aborting in-flight preparation")
}

/* ------------------------ replay instance lifecycle ------------------------- */

// dryRunInstanceAction reports what ensureReplayUp WOULD do for a mod, without
// touching anything -- used by the dry-run log line.
func dryRunInstanceAction(mod string) instanceAction {
	if rp == nil {
		return instStart
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return replayInstanceAction(rp.etCmd != nil, rp.fsGame, mod)
}

// ensureReplayUp makes sure the replay instance is up AND running the mod that
// recorded the demo, restarting it if the warm instance runs a different one,
// then waits for its pipeline hello, honouring an abort. Never affects the live
// instance.
func ensureReplayUp(mod string, timeout time.Duration, cancel <-chan struct{}) error {
	if rp == nil {
		return errStr("replay instance not configured")
	}
	if mod == "" {
		return errStr("no mod resolved for this demo -- refusing to start the replay instance")
	}
	rp.mu.Lock()
	switch act := replayInstanceAction(rp.etCmd != nil, rp.fsGame, mod); act {
	case instReuse:
		rp.mu.Unlock()
	default:
		if act == instRestart {
			log.Printf("replay: warm instance runs mod %q but this demo needs %q -- restarting",
				rp.fsGame, mod)
			rp.kill() // PID-only; never touches the live process
		}
		args := buildReplayArgs(cfg.EtArgs, cfg.LiveHomepath, mod, cfg.ReplayProfile,
			cfg.ReplayTitle, replayPort)
		log.Printf("replay: starting instance (mod %s): %s %s", mod, cfg.EtPath, strings.Join(args, " "))
		// Drop any state from the previous process before spawning: a stale
		// pipeUp/tele would make the caller send `wtvdemo` into a dead socket
		// or believe playback is already active.
		rp.pipeUp = false
		rp.pipeCaps = nil
		rp.tele = Telemetry{}
		rp.fsGame = mod
		rp.vidRestarted = false // fresh process -> vid_restart again after hello
		if err := rp.spawn(args); err != nil {
			rp.fsGame = ""
			rp.mu.Unlock()
			return err
		}
		rp.mu.Unlock()
	}

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
			ensureReplayVidRestart(cancel)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errStr("replay pipeline hello timeout")
}

// ensureReplayVidRestart sends vid_restart to the replay instance exactly once
// per process lifetime, right after its first hello and BEFORE any demo loads
// (the live scene is still on air). A freshly created ET profile has no saved
// config, so resolution cvars (r_mode/r_customwidth/...) come up latched at the
// wrong value until a vid_restart applies the ones passed on the command line.
// Doing it here makes the replay window independent of what the profile
// contains. It never touches the live instance. The socket is engine-level and
// survives the renderer restart; a short settle wait lets the window come back.
func ensureReplayVidRestart(cancel <-chan struct{}) {
	if rp == nil {
		return
	}
	rp.mu.Lock()
	need := !rp.vidRestarted && rp.pipeUp
	if need {
		rp.vidRestarted = true
		rp.pipeExecLocked("vid_restart")
	}
	rp.mu.Unlock()
	if !need {
		return
	}
	log.Println("replay: vid_restart after hello (make resolution independent of the profile config)")
	// let the renderer come back before we start driving the demo.
	select {
	case <-time.After(3 * time.Second):
	case <-cancel:
	}
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
				rp.fsGame = "" // next replay cold-starts with its own demo's mod
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

	// which mod recorded this demo? only that mod can play it back
	mod, modSource, err := resolveSegmentMod(seg)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	// what we hand to `wtvdemo`: "<mod>/<file>" under the demo root. It leaves
	// here as a console argument, so it is validated on this side too.
	relPath := demoPlayArg(seg)
	if !validDemoPath(relPath) {
		writeJSON(w, 400, map[string]string{"error": "refusing to load demo with an unsafe path: " + relPath})
		return
	}

	// locate the demo on disk, under the shared homepath
	absPath, err := demoAbsPath(cfg.LiveHomepath, cfg.ReplayDemoDir, relPath)
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
		file: seg.File, path: relPath, mod: mod, absPath: absPath, offsetMs: offsetMs,
		preMs: pre * 1000, postMs: post * 1000, speed: speed,
	}
	if !replay.begin(job) {
		writeJSON(w, 409, map[string]string{"error": "a replay is already running"})
		return
	}

	// eta based on current instance state and the seek/window math. A warm
	// instance running a different mod has to be restarted, so it costs the
	// same as a cold start.
	rp.mu.Lock()
	instUp := rp.etCmd != nil && rp.pipeUp
	action := replayInstanceAction(rp.etCmd != nil, rp.fsGame, mod)
	rp.mu.Unlock()
	windowStart, windowEnd := playbackWindow(offsetMs, job.preMs, job.postMs)
	ffTarget := fastForwardUntilMs(windowStart, seekMarginMs)
	eta := etaSeconds(instUp && action == instReuse, ffTarget, cfg.ReplaySeekTimescale,
		windowEnd-windowStart, speed)

	go replay.run(job)

	log.Printf("replay: accepted %s (mod %s via %s, instance: %s) offset %dms (pre %ds post %ds speed %.2f) eta %ds",
		relPath, mod, modSource, action, offsetMs, pre, post, speed, eta)
	writeJSON(w, 200, map[string]any{
		"ok": true, "file": seg.File, "path": relPath, "mod": mod, "mod_source": modSource,
		"instance_action": action.String(), "offset_ms": offsetMs, "eta_sec": eta,
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
	// The replay instance MUST share the live homepath: that is where the pk3s
	// (maps + mod) the demos reference were downloaded to. Without them
	// playback dies with a checksum error, so an unknown live homepath is fatal
	// to the replay path -- but never to the live broadcast.
	if cfg.LiveHomepath == "" {
		log.Println("replay: DISABLED -- live_homepath is unknown (set it, or +set fs_homepath in et_args)")
		cfg.ReplayEnabled = false
		return
	}
	replayPort = port
	rp = &instance{
		name:       "replay",
		homepath:   cfg.LiveHomepath, // shared with live, on purpose
		pipeAddr:   cfg.ReplayPipeAddr,
		directs:    false, // the replay orchestrator drives the camera, not the director
		feedEvents: false, // replayed demos must NOT pollute the live event feed
	}
	go rp.pipeLoop()
	go replayIdleMonitor()
	log.Printf("replay: enabled -- instance pipe %s, shared homepath %s, profile %s, demo dir %s, dry_run %v",
		cfg.ReplayPipeAddr, cfg.LiveHomepath, cfg.ReplayProfile, cfg.ReplayDemoDir, cfg.DryRun)
	if risks := sharedHomepathRisks(cfg.EtArgs); len(risks) > 0 {
		log.Printf("replay: NOTE both instances write the same etconfig.cfg (cl_profile is CVAR_ROM "+
			"and is empty by the time ET writes the config). The replay instance forces %s; "+
			"pin them in et_args (+set ...) so the live instance is not affected by what the "+
			"replay instance persists.", strings.Join(risks, ", "))
	}
}
