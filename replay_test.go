package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOffsetFromSvtime(t *testing.T) {
	if got := offsetFromSvtime(123456, 100000); got != 23456 {
		t.Fatalf("offset = %d, want 23456", got)
	}
	// a highlight exactly at segment start is offset 0
	if got := offsetFromSvtime(50000, 50000); got != 0 {
		t.Fatalf("offset = %d, want 0", got)
	}
}

func TestSvtimeInSegment(t *testing.T) {
	closed := DemoSegment{StartSv: 1000, EndSv: 5000}
	open := DemoSegment{StartSv: 1000, EndSv: 0} // still recording

	cases := []struct {
		name string
		seg  DemoSegment
		sv   int
		want bool
	}{
		{"before start", closed, 999, false},
		{"at start", closed, 1000, true},
		{"inside", closed, 3000, true},
		{"at end", closed, 5000, true},
		{"after end", closed, 5001, false},
		{"open before", open, 500, false},
		{"open after start", open, 9_000_000, true},
	}
	for _, c := range cases {
		if got := svtimeInSegment(c.sv, c.seg); got != c.want {
			t.Errorf("%s: svtimeInSegment(%d) = %v, want %v", c.name, c.sv, got, c.want)
		}
	}
}

func TestPlaybackWindow(t *testing.T) {
	// normal window
	s, e := playbackWindow(30000, 8000, 5000)
	if s != 22000 || e != 35000 {
		t.Fatalf("window = [%d,%d], want [22000,35000]", s, e)
	}
	// clamp: pre reaches before the demo start
	s, e = playbackWindow(3000, 8000, 5000)
	if s != 0 {
		t.Fatalf("clamped start = %d, want 0", s)
	}
	if e != 8000 {
		t.Fatalf("end = %d, want 8000", e)
	}
}

func TestFastForwardUntilMs(t *testing.T) {
	if got := fastForwardUntilMs(22000, 1500); got != 20500 {
		t.Fatalf("ff target = %d, want 20500", got)
	}
	// clamp at zero when the window starts very early
	if got := fastForwardUntilMs(500, 1500); got != 0 {
		t.Fatalf("ff target = %d, want 0", got)
	}
}

func TestWallMsForDemoMs(t *testing.T) {
	// at timescale 8, 16000ms of demo takes 2000ms of wall time
	if got := wallMsForDemoMs(16000, 8); got != 2000 {
		t.Fatalf("wall = %d, want 2000", got)
	}
	// slow-mo: at 0.4x, 13000ms of demo takes 32500ms of wall time
	if got := wallMsForDemoMs(13000, 0.4); got != 32500 {
		t.Fatalf("wall = %d, want 32500", got)
	}
	// guard: non-positive timescale treated as 1x, negative demo as 0
	if got := wallMsForDemoMs(1000, 0); got != 1000 {
		t.Fatalf("wall = %d, want 1000", got)
	}
	if got := wallMsForDemoMs(-5, 2); got != 0 {
		t.Fatalf("wall = %d, want 0", got)
	}
}

func TestEtaSeconds(t *testing.T) {
	// warm instance: ff 20500ms@8x = ~2.56s, window 13000ms@0.4x = 32.5s -> ceil 36
	warm := etaSeconds(true, 20500, 8, 13000, 0.4)
	if warm != 36 {
		t.Fatalf("warm eta = %d, want 36", warm)
	}
	// cold instance adds the startup cost
	cold := etaSeconds(false, 20500, 8, 13000, 0.4)
	if cold != warm+replayStartupEtaSec {
		t.Fatalf("cold eta = %d, want %d", cold, warm+replayStartupEtaSec)
	}
}

func TestPortFromAddr(t *testing.T) {
	cases := []struct {
		addr string
		port int
		ok   bool
	}{
		{"127.0.0.1:8791", 8791, true},
		{"0.0.0.0:8788", 8788, true},
		{"noport", 0, false},
		{"host:", 0, false},
		{"host:12x4", 0, false},
	}
	for _, c := range cases {
		p, ok := portFromAddr(c.addr)
		if p != c.port || ok != c.ok {
			t.Errorf("portFromAddr(%q) = (%d,%v), want (%d,%v)", c.addr, p, ok, c.port, c.ok)
		}
	}
}

func TestSplitDemoPath(t *testing.T) {
	cases := []struct {
		p, mod, file string
	}{
		{"silent/wtv_oasis_1.dm_84", "silent", "wtv_oasis_1.dm_84"},
		{"wtv_oasis_1.dm_84", "", "wtv_oasis_1.dm_84"},
		{"a/b/c.dm_84", "", "a/b/c.dm_84"}, // too deep: no split, rejected by validDemoPath
		{"/x.dm_84", "", "x.dm_84"},        // leading slash -> empty mod
	}
	for _, c := range cases {
		mod, file := splitDemoPath(c.p)
		if mod != c.mod || file != c.file {
			t.Errorf("splitDemoPath(%q) = (%q,%q), want (%q,%q)", c.p, mod, file, c.mod, c.file)
		}
	}
}

func TestResolveSegmentMod(t *testing.T) {
	// 1) the client reported the mod -- always wins
	seg := DemoSegment{File: "wtv_oasis_1.dm_84", Path: "silent/wtv_oasis_1.dm_84", Mod: "silent"}
	mod, src, err := resolveSegmentMod(seg)
	if err != nil || mod != "silent" || src != "field" {
		t.Fatalf("reported mod = (%q,%q,%v), want (silent,field,nil)", mod, src, err)
	}
	// the field wins even if the path disagrees (the client is authoritative)
	seg = DemoSegment{File: "wtv_oasis_1.dm_84", Path: "etpub/wtv_oasis_1.dm_84", Mod: "silent"}
	if mod, src, _ = resolveSegmentMod(seg); mod != "silent" || src != "field" {
		t.Fatalf("field should win, got (%q,%q)", mod, src)
	}
	// 2) no field -> the mod directory of the reported path
	seg = DemoSegment{File: "wtv_te_escape2_42.dm_84", Path: "no_quarter/wtv_te_escape2_42.dm_84"}
	mod, src, err = resolveSegmentMod(seg)
	if err != nil || mod != "no_quarter" || src != "directory" {
		t.Fatalf("path mod = (%q,%q,%v), want (no_quarter,directory,nil)", mod, src, err)
	}
	// 3) neither -> a clear error, and no guessed mod. A bare path carries no
	//    mod, and the filename is NOT parsed: wtv_<map>_<svtime> has no mod in it.
	for _, bad := range []DemoSegment{
		{File: "wtv_supply_42.dm_84"},
		{File: "wtv_supply_42.dm_84", Path: "wtv_supply_42.dm_84"},
		{File: "wtv_supply_42.dm_84", Path: "../wtv_supply_42.dm_84"},
	} {
		mod, src, err = resolveSegmentMod(bad)
		if err == nil {
			t.Errorf("%+v: expected an error when no mod is reported", bad)
		}
		if mod != "" || src != "" {
			t.Errorf("%+v: got (%q,%q), want empty on error", bad, mod, src)
		}
	}
	if _, _, err = resolveSegmentMod(DemoSegment{File: "wtv_supply_42.dm_84"}); err == nil ||
		!strings.Contains(err.Error(), "wtv_supply_42.dm_84") {
		t.Errorf("error should name the demo, got %v", err)
	}
}

func TestReplayInstanceAction(t *testing.T) {
	cases := []struct {
		name    string
		running bool
		cur     string
		want    string
		action  instanceAction
	}{
		{"cold start", false, "", "silent", instStart},
		{"cold start, stale mod recorded", false, "silent", "silent", instStart},
		{"warm, same mod", true, "silent", "silent", instReuse},
		{"warm, same mod different case", true, "Silent", "silent", instReuse},
		{"warm, other mod -> restart", true, "silent", "etpub", instRestart},
		{"warm, mod unknown -> restart", true, "", "silent", instRestart},
	}
	for _, c := range cases {
		if got := replayInstanceAction(c.running, c.cur, c.want); got != c.action {
			t.Errorf("%s: action = %v, want %v", c.name, got, c.action)
		}
	}
}

func TestDemoLoadCommand(t *testing.T) {
	// the demo-root-relative path, loaded through wtvdemo (NOT `demo`, which is
	// fs_game-relative and would never find a demo recorded under another mod)
	if got := demoLoadCommand("silent/wtv_supply_1.dm_84"); got != "wtvdemo silent/wtv_supply_1.dm_84" {
		t.Fatalf("load command = %q", got)
	}
}

func TestDemoPlayArg(t *testing.T) {
	// the reported path is passed through verbatim
	seg := DemoSegment{File: "wtv_supply_1.dm_84", Path: "silent/wtv_supply_1.dm_84"}
	if got := demoPlayArg(seg); got != "silent/wtv_supply_1.dm_84" {
		t.Errorf("play arg = %q, want the reported path", got)
	}
	// no path reported -> the bare basename, which wtvdemo also accepts
	seg = DemoSegment{File: "wtv_supply_1.dm_84"}
	if got := demoPlayArg(seg); got != "wtv_supply_1.dm_84" {
		t.Errorf("play arg = %q, want the basename", got)
	}
}

func TestValidDemoPath(t *testing.T) {
	ok := []string{
		"silent/wtv_supply_1.dm_84",         // <mod>/<file>
		"wtv_supply_1.dm_84",                // bare filename
		"no_quarter/wtv_te_escape2_9.dm_84", // underscores in both segments
		"a-b/c-d.dm_84",
	}
	bad := []string{
		"", ".", "..",
		"a/b/c.dm_84",         // more than one directory level
		"../etconfig.cfg",     // traversal
		"silent/../../secret", // traversal inside a segment
		"/x.dm_84",            // absolute
		`C:\demos\x.dm_84`,    // drive letter + backslashes
		`silent\wtv_x.dm_84`,  // backslash separator
		"silent/",             // empty file segment
		"/silent/x.dm_84",     // leading slash
		"x.dm_84; quit",       // console metacharacters
		"x.dm_84\nquit",
		"wtv x.dm_84", // space
	}
	for _, f := range ok {
		if !validDemoPath(f) {
			t.Errorf("validDemoPath(%q) = false, want true", f)
		}
	}
	for _, f := range bad {
		if validDemoPath(f) {
			t.Errorf("validDemoPath(%q) = true, want false", f)
		}
	}
}

func TestDemoAbsPath(t *testing.T) {
	// an unknown live homepath is an error (agent cannot find demos at all)
	if _, err := demoAbsPath("", "wtvdemos", "silent/x.dm_84"); err == nil {
		t.Fatal("expected error for empty live homepath")
	}
	// <live_homepath>/<replay_demo_dir>/<mod>/<file>
	abs, err := demoAbsPath(string(filepath.Separator)+"live", "wtvdemos", "silent/wtv_supply_1.dm_84")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filepath.IsAbs(abs) {
		t.Fatalf("path %q is not absolute", abs)
	}
	// (filepath.Abs qualifies a rooted path with the current drive on Windows)
	want, _ := filepath.Abs(filepath.Join(string(filepath.Separator)+"live", "wtvdemos", "silent", "wtv_supply_1.dm_84"))
	if abs != want {
		t.Fatalf("path = %q, want %q", abs, want)
	}
	// the forward slash from the engine must not survive into the OS path
	if filepath.Separator != '/' && strings.Contains(abs, "/") {
		t.Errorf("path %q still contains a forward slash", abs)
	}
	// empty demo dir falls back to the client default
	abs, _ = demoAbsPath(string(filepath.Separator)+"live", "", "silent/x.dm_84")
	if !strings.Contains(abs, "wtvdemos") {
		t.Fatalf("default demo dir missing from %q", abs)
	}
}

// TestDemoAbsPathOnDisk is the check that matters on the streaming host: the
// path the agent builds from a client-reported "<mod>/<file>" -- which uses a
// forward slash on every platform -- must actually stat. Run under the Windows
// toolchain this exercises real Windows path handling instead of assuming
// filepath normalises the separator.
func TestDemoAbsPathOnDisk(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "wtvdemos", "silent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "wtv_axislab_final_45318300.dm_84"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("demo"), 0o644); err != nil {
		t.Fatal(err)
	}

	abs, err := demoAbsPath(home, "wtvdemos", "silent/"+name)
	if err != nil {
		t.Fatalf("demoAbsPath: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("stat %q (built from a forward-slash path): %v", abs, err)
	}
	t.Logf("%s: resolved and stat'd %q", runtime.GOOS, abs)

	// a demo whose mod directory does not exist must be reported missing
	other, err := demoAbsPath(home, "wtvdemos", "jaymod/"+name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err == nil {
		t.Fatalf("%q should not stat", other)
	}
}

func TestSetArg(t *testing.T) {
	// replace an existing +set pair
	got := setArg([]string{"+set", "cl_wtvPort", "8790"}, "cl_wtvPort", "8791")
	if len(got) != 3 || got[2] != "8791" {
		t.Fatalf("setArg replace = %v", got)
	}
	// append when absent
	got = setArg([]string{"+set", "r_mode", "-1"}, "cl_wtvTitle", "WolfTV-Replay")
	if len(got) != 6 || got[3] != "+set" || got[4] != "cl_wtvTitle" || got[5] != "WolfTV-Replay" {
		t.Fatalf("setArg append = %v", got)
	}
}

func TestStripArg(t *testing.T) {
	got := stripArg([]string{"+set", "name", "Bot", "+connect", "1.2.3.4", "+set", "x", "y"}, "+connect")
	want := []string{"+set", "name", "Bot", "+set", "x", "y"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stripArg = %v, want %v", got, want)
	}
}

func TestBuildReplayArgs(t *testing.T) {
	base := []string{
		"+set", "r_mode", "-1",
		"+set", "cl_wtvPort", "8790",
		"+set", "cl_wtvTitle", "WolfTV-1",
		"+set", "fs_game", "silent",
		"+connect", "1.2.3.4:27960",
	}
	got := buildReplayArgs(base, "C:\\Stream\\livehome", "etpub", "wolftv-replay", "WolfTV-Replay", 8791)
	joined := strings.Join(got, " ")

	// the replay instance must not auto-connect
	if strings.Contains(joined, "+connect") {
		t.Errorf("replay args still contain +connect: %v", got)
	}
	// its own port, title and recording OFF
	assertArg(t, got, "cl_wtvPort", "8791")
	assertArg(t, got, "cl_wtvTitle", "WolfTV-Replay")
	assertArg(t, got, "cl_wtvDemo", "0")
	// the SHARED homepath -- the replay instance needs the live instance's pk3s
	assertArg(t, got, "fs_homepath", "C:\\Stream\\livehome")
	// ...and the mod of the DEMO, not the live one
	assertArg(t, got, "fs_game", "etpub")
	// separated from live by profile, with an explicit pid file (cl_profile is
	// CVAR_ROM and does not survive CL_Init)
	assertArg(t, got, "cl_profile", "wolftv-replay")
	assertArg(t, got, "com_pidfile", "profiles/wolftv-replay/profile.pid")
	// shared-homepath guards
	assertArg(t, got, "s_initsound", "0")
	assertArg(t, got, "db_mode", "1")
	assertArg(t, got, "logfile", "0")
	// unrelated args are preserved
	assertArg(t, got, "r_mode", "-1")

	// the base slice must not be mutated out from under the caller in a way
	// that corrupts the live args (cl_wtvPort and fs_game stay live's values)
	if base[5] != "8790" {
		t.Errorf("base cl_wtvPort mutated to %q", base[5])
	}
	if base[11] != "silent" {
		t.Errorf("base fs_game mutated to %q", base[11])
	}
}

func TestSharedHomepathRisks(t *testing.T) {
	// live pins nothing -> every forced cvar is a risk
	if got := sharedHomepathRisks(nil); len(got) != len(replayForcedCvars) {
		t.Fatalf("risks = %v, want all of %v", got, replayForcedCvars)
	}
	// live pins them all -> nothing to warn about
	var pinned []string
	for _, k := range replayForcedCvars {
		pinned = append(pinned, "+set", k, "1")
	}
	if got := sharedHomepathRisks(pinned); len(got) != 0 {
		t.Fatalf("risks = %v, want none", got)
	}
	// partial: only the unpinned ones are reported
	got := sharedHomepathRisks([]string{"+set", "cl_wtvDemo", "1", "+set", "s_initsound", "1"})
	for _, k := range got {
		if k == "cl_wtvDemo" || k == "s_initsound" {
			t.Errorf("pinned cvar %q reported as a risk", k)
		}
	}
	if len(got) != len(replayForcedCvars)-2 {
		t.Fatalf("risks = %v, want %d entries", got, len(replayForcedCvars)-2)
	}
}

func TestHighlightLabel(t *testing.T) {
	cases := []struct {
		h    Highlight
		want string
	}{
		{Highlight{Kind: "multikill", Count: 2, Player: "Rob"}, "Double kill by Rob"},
		{Highlight{Kind: "multikill", Count: 3, Player: "Rob"}, "Triple kill by Rob"},
		{Highlight{Kind: "multikill", Count: 6, Player: "Rob"}, "6-kill by Rob"},
		{Highlight{Kind: "dynamite"}, "Dynamite"},
		{Highlight{Kind: "objective", Player: "Ann"}, "Objective by Ann"},
	}
	for _, c := range cases {
		if got := highlightLabel(c.h); got != c.want {
			t.Errorf("label(%+v) = %q, want %q", c.h, got, c.want)
		}
	}
}

func TestSegmentViews(t *testing.T) {
	segs := []DemoSegment{
		// pre-change segment: no mod field, no mod directory -> unknowable
		{File: "wtv_supply_0.dm_84", Map: "supply", StartSv: 0, EndSv: 10000},
		// current segment: the mod is the directory of the reported path
		{File: "wtv_goldrush_20000.dm_84", Path: "silent/wtv_goldrush_20000.dm_84",
			Map: "goldrush", StartSv: 20000, EndSv: 30000},
	}
	hls := []Highlight{
		{Kind: "multikill", Count: 3, Player: "Rob", SvTime: 5000, Score: 9}, // in old
		{Kind: "dynamite", SvTime: 25000, Score: 8},                          // in new
		{Kind: "objective", Player: "Ann", SvTime: 99999, Score: 5},          // in neither
	}
	views := segmentViews(segs, hls)
	if len(views) != 2 {
		t.Fatalf("want 2 views, got %d", len(views))
	}
	// newest first
	if views[0].File != "wtv_goldrush_20000.dm_84" {
		t.Fatalf("views[0] = %s, want the newest segment first", views[0].File)
	}
	// each segment carries the mod needed to replay it, and where it came from
	if views[0].Mod != "silent" || views[0].ModSource != "directory" {
		t.Errorf("views[0] mod = (%q,%q), want (silent,directory)", views[0].Mod, views[0].ModSource)
	}
	// ...and the path handed to wtvdemo verbatim
	if views[0].Path != "silent/wtv_goldrush_20000.dm_84" {
		t.Errorf("views[0] path = %q", views[0].Path)
	}
	if views[1].Mod != "" || views[1].ModSource != "unknown" {
		t.Errorf("views[1] mod = (%q,%q), want (,unknown)", views[1].Mod, views[1].ModSource)
	}
	if len(views[0].Highlights) != 1 || views[0].Highlights[0].Kind != "dynamite" {
		t.Fatalf("new segment highlights = %+v", views[0].Highlights)
	}
	// offset is relative to that segment's start
	if views[0].Highlights[0].OffsetMs != 5000 {
		t.Fatalf("dynamite offset = %d, want 5000", views[0].Highlights[0].OffsetMs)
	}
	if len(views[1].Highlights) != 1 || views[1].Highlights[0].OffsetMs != 5000 {
		t.Fatalf("old segment highlights = %+v", views[1].Highlights)
	}
	// the out-of-range highlight is in no segment
	for _, v := range views {
		for _, h := range v.Highlights {
			if h.Kind == "objective" {
				t.Fatal("out-of-range objective leaked into a segment")
			}
		}
	}
}

func TestArgValue(t *testing.T) {
	args := []string{"+set", "fs_game", "silent", "+set", "cl_wtvPort", "8790"}
	if got := argValue(args, "fs_game"); got != "silent" {
		t.Errorf("fs_game = %q, want silent", got)
	}
	if got := argValue(args, "cl_wtvPort"); got != "8790" {
		t.Errorf("cl_wtvPort = %q, want 8790", got)
	}
	if got := argValue(args, "missing"); got != "" {
		t.Errorf("missing = %q, want empty", got)
	}
}

// TestReplayRunDryRun drives the whole orchestration in dry-run mode (no ET,
// no OBS) and asserts it completes and returns to idle -- i.e. finish() always
// runs and the single-replay lock is released.
func TestReplayRunDryRun(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.DryRun = true
	cfg.ReplaySeekTimescale = 8
	cfg.ReplaySpeed = 0.4
	cfg.SceneLive = "Live"
	cfg.SceneReplay = "Replay"

	replay = replayController{}
	job := replayJob{
		file: "wtv_supply_1.dm_84", path: "silent/wtv_supply_1.dm_84", mod: "silent",
		absPath:  "C:\\live\\wtvdemos\\silent\\wtv_supply_1.dm_84",
		offsetMs: 45000, preMs: 8000, postMs: 5000, speed: 0.4,
	}

	if !replay.begin(job) {
		t.Fatal("begin should succeed on an idle controller")
	}
	if replay.begin(job) {
		t.Fatal("begin should return false while a replay is active (409 path)")
	}

	replay.run(job) // dry-run: fast, synchronous

	st := replay.status()
	if st["active"].(bool) {
		t.Fatal("replay still active after run")
	}
	if st["phase"].(string) != "idle" {
		t.Fatalf("phase = %v, want idle", st["phase"])
	}
	if replay.lastDone.IsZero() {
		t.Fatal("lastDone not set -- idle-stop timer would never fire")
	}
}

// TestReplayAbortReturnsToLive verifies that aborting mid-run still ends idle
// (finish() cuts back to live regardless of where the abort landed).
func TestReplayAbortReturnsToLive(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.DryRun = true
	cfg.ReplaySeekTimescale = 8
	cfg.ReplaySpeed = 0.4
	cfg.SceneLive = "Live"
	cfg.SceneReplay = "Replay"

	replay = replayController{}
	job := replayJob{file: "x.dm_84", path: "silent/x.dm_84", mod: "silent",
		absPath: "C:\\x.dm_84", offsetMs: 45000, preMs: 8000, postMs: 5000, speed: 0.4}
	if !replay.begin(job) {
		t.Fatal("begin failed")
	}
	done := make(chan struct{})
	go func() { replay.run(job); close(done) }()
	replay.stop() // abort immediately
	<-done

	st := replay.status()
	if st["active"].(bool) {
		t.Fatal("replay still active after abort")
	}
	if st["phase"].(string) != "idle" {
		t.Fatalf("phase = %v, want idle after abort", st["phase"])
	}
}

func assertArg(t *testing.T, args []string, key, want string) {
	t.Helper()
	if got := argValue(args, key); got != want {
		t.Errorf("arg %s = %q, want %q (in %v)", key, got, want, args)
	}
}
