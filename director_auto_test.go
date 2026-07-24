package main

import (
	"testing"
	"time"
)

const tNow = int64(1_000_000_000_000)

func segOpen(file, mod, mp string, start int) DemoSegment {
	return DemoSegment{File: file, Path: mod + "/" + file, Mod: mod, Map: mp, StartSv: start}
}

func replHL(mp string, slot, sv, score int, ageMs int64) Highlight {
	return Highlight{Kind: "multikill", Player: "P", Map: mp, SvTime: sv, Score: score,
		FollowedSlot: slot, PlayerSlot: slot, Replayable: true, Recv: tNow - ageMs}
}

func TestBuildAutoCandidates(t *testing.T) {
	segs := []DemoSegment{
		segOpen("wtv_goldrush_1000.dm_84", "silent", "goldrush", 1000),
		segOpen("wtv_supply_2000.dm_84", "silent", "supply", 2000), // other map
	}
	hls := []Highlight{
		replHL("goldrush", 5, 5000, 9, 70_000), // replayable, in seg 1
		{Kind: "multikill", Map: "goldrush", SvTime: 5200, Score: 6, FollowedSlot: 9, PlayerSlot: 3, Replayable: false, Recv: tNow}, // not replayable
		replHL("supply", 1, 2500, 6, 70_000), // replayable but other map
	}
	cands := buildAutoCandidates(segs, hls, tNow, "goldrush")
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate (replayable, current map), got %d: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.File != "wtv_goldrush_1000.dm_84" || c.OffsetMs != 4000 || c.PlayerSlot != 5 || c.Mod != "silent" {
		t.Errorf("candidate wrong: %+v", c)
	}
}

func TestSelectAutoCandidate(t *testing.T) {
	dc := DirectorConfig{MinHighlightAgeSec: 60, PerMapCap: 3}
	// two eligible: a triple (score 9) and a double (6). triple wins.
	cands := []autoCandidate{
		{File: "a", SvTime: 1, PlayerSlot: 1, Score: 6, AgeMs: 70_000},
		{File: "b", SvTime: 2, PlayerSlot: 2, Score: 9, AgeMs: 70_000},
	}
	got, ok := selectAutoCandidate(cands, dc, -1, map[string]bool{}, tNow)
	if !ok || got.File != "b" {
		t.Fatalf("want highest score b, got %+v", got)
	}
	// never the same player twice in a row: exclude slot 2 -> falls back to a.
	got, ok = selectAutoCandidate(cands, dc, 2, map[string]bool{}, tNow)
	if !ok || got.File != "a" {
		t.Fatalf("same-player-in-a-row not excluded: %+v", got)
	}
	// never the same highlight twice: mark b used -> a.
	used := map[string]bool{candKey(cands[1]): true}
	got, ok = selectAutoCandidate(cands, dc, -1, used, tNow)
	if !ok || got.File != "a" {
		t.Fatalf("used highlight not excluded: %+v", got)
	}
	// too-young highlight (still being written) is not eligible.
	young := []autoCandidate{{File: "c", SvTime: 3, PlayerSlot: 3, Score: 9, AgeMs: 10_000}}
	if _, ok := selectAutoCandidate(young, dc, -1, map[string]bool{}, tNow); ok {
		t.Fatal("a highlight younger than min age must not be selected")
	}
}

func TestCandidateEligibleAgeBounds(t *testing.T) {
	dc := DirectorConfig{MinHighlightAgeSec: 60}
	c := autoCandidate{File: "a", PlayerSlot: 1, Score: 6}
	c.AgeMs = 59_000
	if candidateEligible(c, dc, -1, map[string]bool{}) {
		t.Error("below min age should be ineligible")
	}
	c.AgeMs = 61_000
	if !candidateEligible(c, dc, -1, map[string]bool{}) {
		t.Error("just above min age should be eligible")
	}
	c.AgeMs = int64(autoMaxHighlightAgeSec)*1000 + 1000
	if candidateEligible(c, dc, -1, map[string]bool{}) {
		t.Error("beyond the horizon should be ineligible")
	}
}

func TestQuietForMs(t *testing.T) {
	f := newTestFeed()
	f.events = []ActionEvent{
		{Kind: "chat", Recv: tNow - 1000},
		{Kind: "kill", Recv: tNow - 4000},
		{Kind: "chat", Recv: tNow - 500}, // chat does not break a lull
	}
	if q := f.quietForMs(tNow); q != 4000 {
		t.Errorf("quietForMs = %d, want 4000 (last kill, chat ignored)", q)
	}
	f2 := newTestFeed()
	f2.events = []ActionEvent{{Kind: "chat", Recv: tNow - 100}}
	if q := f2.quietForMs(tNow); q < int64(1<<40) {
		t.Errorf("no action events should read as effectively infinite quiet, got %d", q)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestAutoPrepareTriggerDryRun(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.DryRun = true
	cfg.ReplaySeekTimescale = 8
	cfg.ReplaySeekMode = "timescale"
	cfg.SceneLive = "Live"
	cfg.SceneReplay = "Replay"

	replay = replayController{}
	job := replayJob{file: "wtv_goldrush_1.dm_84", path: "silent/wtv_goldrush_1.dm_84", mod: "silent",
		absPath: "C:\\x", offsetMs: 45000, preMs: 8000, postMs: 5000, speed: 0.4}

	if !replay.prepareAuto(job) {
		t.Fatal("prepareAuto should claim the idle slot")
	}
	waitFor(t, 3*time.Second, func() bool { return replay.isPrepared() })
	if !replay.isActive() {
		t.Fatal("slot should stay claimed while holding")
	}
	if pv := replay.preparedView(); pv == nil || pv["held"] != true {
		t.Fatalf("preparedView should report a held clip: %+v", pv)
	}

	if !replay.triggerPrepared() {
		t.Fatal("triggerPrepared should succeed when prepared")
	}
	waitFor(t, 3*time.Second, func() bool { return !replay.isActive() })
	if replay.status()["phase"].(string) != "idle" {
		t.Fatalf("phase should be idle after the triggered replay finishes")
	}
	// triggering again with nothing prepared must be a no-op.
	if replay.triggerPrepared() {
		t.Fatal("triggerPrepared should be false when nothing is prepared")
	}
}

// BUG 2: stop() on a HELD preparation (no goroutine on cancel) must tear it
// down, not leave the slot leaked.
func TestStopTearsDownHeldPreparation(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.DryRun = true
	cfg.SceneLive = "Live"
	cfg.ReplaySeekMode = "timescale"

	replay = replayController{}
	job := replayJob{file: "h.dm_84", path: "silent/h.dm_84", mod: "silent", absPath: "C:\\h",
		offsetMs: 20000, preMs: 8000, postMs: 5000, speed: 0.4}
	if !replay.prepareAuto(job) {
		t.Fatal("prepareAuto failed")
	}
	waitFor(t, 3*time.Second, func() bool { return replay.isPrepared() })
	replay.stop()
	if replay.isActive() {
		t.Fatal("stop() on a held preparation must release the slot")
	}
}

// BUG 1/4: a discarded preparation never aired, so it must not stamp lastAired
// (which would impose the min-interval spacing on the next attempt).
func TestDiscardedPrepDoesNotConsumeInterval(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.DryRun = true
	cfg.SceneLive = "Live"
	cfg.ReplaySeekMode = "timescale"

	replay = replayController{}
	job := replayJob{file: "i.dm_84", path: "silent/i.dm_84", mod: "silent", absPath: "C:\\i",
		offsetMs: 20000, preMs: 8000, postMs: 5000, speed: 0.4}
	if !replay.prepareAuto(job) {
		t.Fatal("prepareAuto failed")
	}
	waitFor(t, 3*time.Second, func() bool { return replay.isPrepared() })
	replay.discardPrepared()
	if !replay.lastAiredTime().IsZero() {
		t.Fatal("a discarded prep must not stamp lastAired")
	}
}

// discardPrepared must never tear down a MANUAL replay (auto flag false).
func TestDiscardLeavesManualReplayAlone(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.DryRun = true
	cfg.SceneLive = "Live"

	replay = replayController{}
	job := replayJob{file: "m.dm_84", path: "silent/m.dm_84", mod: "silent", absPath: "C:\\m",
		offsetMs: 20000, preMs: 8000, postMs: 5000, speed: 0.4}
	if !replay.begin(job) { // manual claim (auto stays false), no goroutine started
		t.Fatal("begin failed")
	}
	replay.discardPrepared()
	if !replay.isActive() {
		t.Fatal("discardPrepared must not touch a manual replay")
	}
	replay.finish(true, false) // cleanup
}

// a triggered replay DID air, so lastAired is stamped (spacing applies).
func TestTriggeredReplaySetsLastAired(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.DryRun = true
	cfg.SceneLive = "Live"
	cfg.SceneReplay = "Replay"
	cfg.ReplaySeekMode = "timescale"

	replay = replayController{}
	job := replayJob{file: "t.dm_84", path: "silent/t.dm_84", mod: "silent", absPath: "C:\\t",
		offsetMs: 20000, preMs: 8000, postMs: 5000, speed: 0.4}
	if !replay.prepareAuto(job) {
		t.Fatal("prepareAuto failed")
	}
	waitFor(t, 3*time.Second, func() bool { return replay.isPrepared() })
	if !replay.triggerPrepared() {
		t.Fatal("trigger failed")
	}
	waitFor(t, 3*time.Second, func() bool { return !replay.isActive() })
	if replay.lastAiredTime().IsZero() {
		t.Fatal("a triggered (aired) replay must stamp lastAired")
	}
}

func TestDiscardPreparedReturnsIdle(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.DryRun = true
	cfg.SceneLive = "Live"
	cfg.ReplaySeekMode = "timescale"

	replay = replayController{}
	job := replayJob{file: "d.dm_84", path: "silent/d.dm_84", mod: "silent", absPath: "C:\\d",
		offsetMs: 20000, preMs: 8000, postMs: 5000, speed: 0.4}
	if !replay.prepareAuto(job) {
		t.Fatal("prepareAuto failed")
	}
	waitFor(t, 3*time.Second, func() bool { return replay.isPrepared() })
	replay.discardPrepared()
	if replay.isActive() {
		t.Fatal("discardPrepared should release the slot")
	}
}
