package main

import (
	"path/filepath"
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

func TestDemoRelPath(t *testing.T) {
	withPath := DemoSegment{File: "wtv_supply_100.dm_84", Path: "wtvdemos/wtv_supply_100.dm_84"}
	if got := demoRelPath(withPath, "wtvdemos"); got != "wtvdemos/wtv_supply_100.dm_84" {
		t.Fatalf("relpath = %q", got)
	}
	// fall back to <demoDir>/<file> when the engine did not report a path
	noPath := DemoSegment{File: "wtv_supply_100.dm_84"}
	if got := demoRelPath(noPath, "customdemos"); got != "customdemos/wtv_supply_100.dm_84" {
		t.Fatalf("relpath = %q", got)
	}
}

func TestDemoAbsPath(t *testing.T) {
	// missing homepath/fsgame is an error (agent can't locate demos)
	if _, err := demoAbsPath("", "silent", "wtvdemos/x.dm_84"); err == nil {
		t.Fatal("expected error for empty live homepath")
	}
	if _, err := demoAbsPath("/home/live", "", "wtvdemos/x.dm_84"); err == nil {
		t.Fatal("expected error for empty fs_game")
	}
	abs, err := demoAbsPath(string(filepath.Separator)+"live", "silent", "wtvdemos/x.dm_84")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filepath.IsAbs(abs) {
		t.Fatalf("path %q is not absolute", abs)
	}
	if !strings.Contains(abs, "silent") || !strings.HasSuffix(abs, "x.dm_84") {
		t.Fatalf("path %q missing expected components", abs)
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
	got := buildReplayArgs(base, "C:\\Stream\\replayhome", "WolfTV-Replay", 8791)
	joined := strings.Join(got, " ")

	// the replay instance must not auto-connect
	if strings.Contains(joined, "+connect") {
		t.Errorf("replay args still contain +connect: %v", got)
	}
	// its own port, title, homepath and recording OFF
	assertArg(t, got, "cl_wtvPort", "8791")
	assertArg(t, got, "cl_wtvTitle", "WolfTV-Replay")
	assertArg(t, got, "fs_homepath", "C:\\Stream\\replayhome")
	assertArg(t, got, "cl_wtvDemo", "0")
	// unrelated args are preserved
	assertArg(t, got, "fs_game", "silent")

	// the base slice must not be mutated out from under the caller in a way
	// that corrupts the live args (cl_wtvPort stays live's value)
	if base[5] != "8790" {
		t.Errorf("base cl_wtvPort mutated to %q", base[5])
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
		{File: "old.dm_84", Map: "supply", StartSv: 0, EndSv: 10000},
		{File: "new.dm_84", Map: "goldrush", StartSv: 20000, EndSv: 30000},
	}
	hls := []Highlight{
		{Kind: "multikill", Count: 3, Player: "Rob", SvTime: 5000, Score: 9},   // in old
		{Kind: "dynamite", SvTime: 25000, Score: 8},                            // in new
		{Kind: "objective", Player: "Ann", SvTime: 99999, Score: 5},            // in neither
	}
	views := segmentViews(segs, hls)
	if len(views) != 2 {
		t.Fatalf("want 2 views, got %d", len(views))
	}
	// newest first
	if views[0].File != "new.dm_84" {
		t.Fatalf("views[0] = %s, want new.dm_84 (newest first)", views[0].File)
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
		file: "wtv_supply_1.dm_84", absPath: "C:\\live\\silent\\wtvdemos\\wtv_supply_1.dm_84",
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
	job := replayJob{file: "x.dm_84", absPath: "C:\\x.dm_84", offsetMs: 45000, preMs: 8000, postMs: 5000, speed: 0.4}
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
