package main

// Crash reporting + auto-restart. Every long-lived goroutine is wrapped so a
// panic ends with a detailed report on disk (crash-<ts>.log next to the agent),
// a one-line entry in the rolling crashes.log, and a re-exec of the agent that
// re-adopts the live ET through the existing /restart handoff. The last crash
// is also surfaced on /status so the panel can show "agent restarted itself".
//
// WHAT recover() CANNOT CATCH: os.Exit, fatal runtime errors ("fatal error:
// concurrent map writes", out-of-memory) and the Go runtime itself dying. For
// those the backstop is running the agent with stderr captured (see README
// "Autostart"): 2> crash.txt. This file is written by the runtime just before
// the process ends; the Task Scheduler restart-on-failure brings the process
// back and adoptLiveET re-attaches to the live ET.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

// CrashInfo is the last-crash summary reported on /status.
type CrashInfo struct {
	Time         time.Time `json:"time"`
	Subsystem    string    `json:"subsystem"`
	Summary      string    `json:"summary"`
	ReportFile   string    `json:"report_file"`
	RestartCount int       `json:"restart_count"`
}

const (
	// crashLoopWindow / crashLoopLimit define the loop breaker: more than
	// crashLoopLimit crashes inside crashLoopWindow stops auto-restart. Set as
	// per Feature 1: "more than 5 crashes in 2 minutes -> stop restarting".
	crashLoopWindow = 2 * time.Minute
	crashLoopLimit  = 5
)

var (
	crashMu    sync.Mutex
	lastCrash  *CrashInfo
	crashTimes []time.Time // wall-clock timestamps of recent crashes for the loop breaker
)

// crashRestartFn is the post-crash restart action. Overridable so tests can
// observe the call without re-execing themselves.
var crashRestartFn = reexecOnCrash

// crashExitFn is called after a crash report + (optional) restart. Overridable
// so tests do not exit the process.
var crashExitFn = func(code int) { os.Exit(code) }

// crashDir is where crash-<ts>.log / crashes.log are written. Overridable in
// tests so they can inspect the files without polluting the repo root. Default
// is the directory of the running executable (falling back to cwd).
var crashDir = ""

// guard runs fn with a deferred recover and, on panic, writes a full crash
// report and triggers the auto-restart path. Use this to wrap the body of any
// long-lived goroutine; goGuarded is the goroutine-launching form.
func guard(subsystem string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			handleCrash(subsystem, r, debug.Stack())
		}
	}()
	fn()
}

// goGuarded starts fn as a new goroutine wrapped in guard(subsystem, fn).
func goGuarded(subsystem string, fn func()) {
	go guard(subsystem, fn)
}

// handleCrash writes the report, appends the rolling log, updates lastCrash and
// -- unless the crash-loop breaker has tripped -- re-execs the agent and exits.
// Safe to call concurrently from multiple crashed goroutines.
func handleCrash(subsystem string, v any, stack []byte) {
	now := time.Now()
	summary := fmt.Sprintf("%v", v)
	snap := captureCrashContext(subsystem)

	reportPath := writeCrashReport(now, subsystem, summary, stack, snap)
	appendCrashesLog(now, subsystem, summary, reportPath)

	crashMu.Lock()
	cutoff := now.Add(-crashLoopWindow)
	kept := crashTimes[:0]
	for _, t := range crashTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	crashTimes = append(kept, now)
	inLoop := len(crashTimes) > crashLoopLimit
	windowCount := len(crashTimes)
	lastCrash = &CrashInfo{
		Time: now, Subsystem: subsystem, Summary: summary,
		ReportFile: reportPath, RestartCount: windowCount,
	}
	crashMu.Unlock()

	log.Printf("CRASH [%s]: %v -- report %s (%d in %v)",
		subsystem, v, reportPath, windowCount, crashLoopWindow)

	if inLoop {
		log.Printf("CRASH LOOP tripped (>%d crashes in %v) -- NOT auto-restarting; "+
			"fix the underlying issue and restart manually", crashLoopLimit, crashLoopWindow)
		return
	}
	if err := crashRestartFn(); err != nil {
		log.Println("CRASH: re-exec failed, staying up so at least this goroutine's death is logged:", err)
		return
	}
	log.Println("CRASH: replacement spawned; exiting so it can adopt the live ET")
	crashExitFn(2)
}

// getLastCrash returns a copy of the last-crash record (nil if none).
func getLastCrash() *CrashInfo {
	crashMu.Lock()
	defer crashMu.Unlock()
	if lastCrash == nil {
		return nil
	}
	c := *lastCrash
	return &c
}

// reexecOnCrash is the default post-crash restart: same as POST /restart. The
// child inherits stdout/stderr (see README on stderr redirection) and the new
// process's adoptLiveET re-attaches to the live ET so the broadcast survives.
func reexecOnCrash() error { return reexec() }

// resetCrashLoopIfStable clears the crash-loop counter once the agent has been
// up long enough that a fresh batch of failures should start counting from zero.
// Called from a background timer.
func resetCrashLoopIfStable() {
	crashMu.Lock()
	defer crashMu.Unlock()
	// Any crashes older than the window are dropped, but keep the last entry
	// visible on /status.
	cutoff := time.Now().Add(-crashLoopWindow)
	kept := crashTimes[:0]
	for _, t := range crashTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	crashTimes = kept
}

// crashLoopMonitor runs a slow ticker to keep the loop-window counter honest
// even if the agent stops crashing (so a stable process eventually stops
// carrying old failures in its restart_count).
func crashLoopMonitor() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		resetCrashLoopIfStable()
	}
}

/* ----------------------- report writers ----------------------- */

// crashDirOrDefault returns crashDir (test override) or the directory of the
// running executable, falling back to cwd.
func crashDirOrDefault() string {
	if crashDir != "" {
		return crashDir
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

// writeCrashReport writes the full per-crash file and returns its path.
func writeCrashReport(t time.Time, subsystem, summary string, stack []byte, snap map[string]any) string {
	name := fmt.Sprintf("crash-%s.log", t.Format("20060102-150405.000"))
	path := filepath.Join(crashDirOrDefault(), name)

	// Deterministic JSON key order for the snapshot so reports are diff-friendly.
	snapJSON := marshalSortedJSON(snap)

	body := fmt.Sprintf(
		"wolftv-agent crash report\n"+
			"time:       %s\n"+
			"subsystem:  %s\n"+
			"panic:      %s\n"+
			"pid:        %d\n"+
			"goversion:  %s\n"+
			"platform:   %s\n\n"+
			"--- stack trace ---\n%s\n"+
			"--- runtime context ---\n%s\n",
		t.Format(time.RFC3339Nano), subsystem, summary,
		os.Getpid(), runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH,
		string(stack), snapJSON,
	)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		log.Println("crash: write report failed:", err)
		return ""
	}
	return path
}

// appendCrashesLog appends one line to the rolling crashes.log.
func appendCrashesLog(t time.Time, subsystem, summary, reportPath string) {
	path := filepath.Join(crashDirOrDefault(), "crashes.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Println("crash: open crashes.log failed:", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\t%s\t%s\t%s\n", t.Format(time.RFC3339), subsystem,
		summary, reportPath)
}

// marshalSortedJSON marshals a map[string]any with sorted top-level keys, so
// crash reports diff cleanly between incidents.
func marshalSortedJSON(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := map[string]json.RawMessage{}
	for _, k := range keys {
		b, err := json.MarshalIndent(m[k], "  ", "  ")
		if err != nil {
			b = []byte(fmt.Sprintf("%q", err.Error()))
		}
		out[k] = b
	}
	// Manual serialisation to preserve key order.
	var buf []byte
	buf = append(buf, '{', '\n')
	for i, k := range keys {
		buf = append(buf, ' ', ' ')
		kb, _ := json.Marshal(k)
		buf = append(buf, kb...)
		buf = append(buf, ':', ' ')
		buf = append(buf, out[k]...)
		if i < len(keys)-1 {
			buf = append(buf, ',')
		}
		buf = append(buf, '\n')
	}
	buf = append(buf, '}', '\n')
	return string(buf)
}

/* --------------------- runtime context snapshot --------------------- */

// captureCrashContext gathers as much runtime state as possible without
// deadlocking. Every mutex is probed with TryLock: a panic may have left its
// lock held (though inline critical sections without a deferred Unlock DO leak
// their lock on panic), and blocking here would hang the whole reporter and
// therefore the restart. If a lock is unavailable, the field is annotated
// "unavailable (lock held)" instead of missing.
func captureCrashContext(subsystem string) map[string]any {
	snap := map[string]any{}
	snap["subsystem"] = subsystem
	snap["captured_at"] = time.Now().Format(time.RFC3339Nano)

	// Runtime totals — cheap, always safe.
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	snap["goroutines"] = runtime.NumGoroutine()
	snap["memory"] = map[string]any{
		"alloc_mb":       mem.Alloc / 1024 / 1024,
		"total_alloc_mb": mem.TotalAlloc / 1024 / 1024,
		"sys_mb":         mem.Sys / 1024 / 1024,
		"num_gc":         mem.NumGC,
	}

	// Live instance state (server, map, telemetry, camera).
	snap["live"] = captureInstanceState(st)
	// Replay instance state (if configured).
	if rp != nil {
		snap["replay_instance"] = captureInstanceState(rp)
	}

	// Replay controller (phase / current file / mod / offset).
	snap["replay"] = captureReplayState()

	// Auto-director / OBS scene / Twitch.
	snap["auto_director"] = captureAutoState()
	snap["obs"] = captureOBSState()
	snap["twitch"] = captureTwitchState()

	// Last N event feed entries — the most valuable single field for the
	// post-replay-death diagnosis. Copy defensively via TryLock.
	snap["feed_recent"] = captureFeedRecent(20)

	return snap
}

// captureInstanceState reads what it can from an instance under TryLock.
func captureInstanceState(in *instance) map[string]any {
	out := map[string]any{"name": in.name}
	if !in.mu.TryLock() {
		out["_note"] = "unavailable (lock held; probably held by the crashed goroutine)"
		return out
	}
	defer in.mu.Unlock()
	out["current_server"] = in.currentServer
	out["fs_game"] = in.fsGame
	out["pipe_up"] = in.pipeUp
	out["adopted"] = in.adopted
	out["desired"] = in.desired
	out["cur_target"] = in.curTarget
	out["cur_target_slot"] = in.curTargetSlot
	out["last_event"] = in.lastEvent.Format(time.RFC3339)
	out["last_switch"] = in.lastSwitch.Format(time.RFC3339)
	out["started_at"] = in.startedAt.Format(time.RFC3339)
	out["tele"] = map[string]any{
		"state":     in.tele.State,
		"server":    in.tele.Server,
		"map":       in.tele.Map,
		"following": in.tele.Following,
		"fps":       in.tele.FPS,
		"percent":   in.tele.Percent,
		"reason":    in.tele.Reason,
	}
	return out
}

// captureReplayState reads the replay controller under TryLock.
func captureReplayState() map[string]any {
	if !replay.mu.TryLock() {
		return map[string]any{"_note": "unavailable (lock held)"}
	}
	defer replay.mu.Unlock()
	return map[string]any{
		"active":        replay.active,
		"phase":         replay.phase,
		"file":          replay.file,
		"mod":           replay.mod,
		"offset_ms":     replay.offsetMs,
		"auto":          replay.auto,
		"auto_prepared": replay.autoPrepared,
		"started_at":    replay.startedAt,
		"last_done":     replay.lastDone.Format(time.RFC3339),
		"last_aired":    replay.lastAired.Format(time.RFC3339),
	}
}

// captureAutoState reads the auto-director state under TryLock.
func captureAutoState() map[string]any {
	if !auto.mu.TryLock() {
		return map[string]any{"_note": "unavailable (lock held)"}
	}
	defer auto.mu.Unlock()
	return map[string]any{
		"cur_map":              auto.curMap,
		"map_replays":          auto.mapReplays,
		"last_player_slot":     auto.lastPlayerSlot,
		"prepared_key":         auto.preparedKey,
		"prepared_player_slot": auto.preparedPlayerSlot,
		"prepared_map":         auto.preparedMap,
		"candidate_count":      len(auto.lastCandidates),
	}
}

// captureOBSState reads OBS connection/scene state cheaply.
func captureOBSState() map[string]any {
	up, streaming := obsStreamStatus()
	return map[string]any{
		"connected": up,
		"streaming": streaming,
		"cfg_addr":  cfg.ObsAddr,
	}
}

// captureTwitchState reads the twitch client under TryLock (nil if disabled).
func captureTwitchState() map[string]any {
	if twitch == nil {
		return map[string]any{"enabled": false}
	}
	if !twitch.mu.TryLock() {
		return map[string]any{"_note": "unavailable (lock held)"}
	}
	defer twitch.mu.Unlock()
	return map[string]any{
		"enabled":      true,
		"disabled":     twitch.disabled,
		"broadcaster":  twitch.broadcasterID,
		"title":        twitch.currentTitle,
		"last_marker":  twitch.lastMarkerAt.Format(time.RFC3339),
		"token_expiry": twitch.tokenExpiry.Format(time.RFC3339),
	}
}

// captureFeedRecent returns up to n most recent ActionEvents from the event
// feed, using TryLock so a held feed.mu does not block the crash report.
func captureFeedRecent(n int) any {
	if !feed.mu.TryLock() {
		return "unavailable (feed lock held)"
	}
	defer feed.mu.Unlock()
	if len(feed.events) == 0 {
		return []ActionEvent{}
	}
	from := len(feed.events) - n
	if from < 0 {
		from = 0
	}
	out := make([]ActionEvent, len(feed.events)-from)
	copy(out, feed.events[from:])
	return out
}
