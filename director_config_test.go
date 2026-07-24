package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyDirectorPatch(t *testing.T) {
	base := DirectorConfig{AutoReplay: false, LullSec: 6, MinIntervalSec: 180, Speed: 0.4}
	lull := 10
	auto := true
	got := applyDirectorPatch(base, directorConfigPatch{LullSec: &lull, AutoReplay: &auto})
	if !got.AutoReplay || got.LullSec != 10 {
		t.Fatalf("patch not applied: %+v", got)
	}
	// untouched fields keep their base value
	if got.MinIntervalSec != 180 || got.Speed != 0.4 {
		t.Errorf("patch clobbered untouched fields: %+v", got)
	}
}

func TestClampDirectorConfig(t *testing.T) {
	// a panel that posts zeros/negatives must not produce a divide-by-zero
	// half-life or a self-wedging (0-interval, 0-speed) config.
	c := clampDirectorConfig(DirectorConfig{
		HeatWindowSec: 0, HeatHalfLifeSec: 0, HeatSpikeFactor: 0,
		LullSec: 0, MinIntervalSec: -5, MinHighlightAgeSec: -1, PerMapCap: -1,
		PreSec: -1, PostSec: -1, Speed: 0,
	})
	if c.HeatWindowSec < 5 || c.HeatHalfLifeSec < 1 || c.HeatSpikeFactor < 1 || c.LullSec < 1 {
		t.Errorf("heat/lull not clamped: %+v", c)
	}
	if c.MinIntervalSec != 0 || c.MinHighlightAgeSec != 0 || c.PerMapCap != 0 {
		t.Errorf("negatives not floored to 0: %+v", c)
	}
	if c.PreSec != 0 || c.PostSec != 0 || c.Speed <= 0 {
		t.Errorf("window not clamped: %+v", c)
	}
}

func TestDirectorConfigPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	old := configPath
	configPath = filepath.Join(dir, "config.json")
	defer func() { configPath = old }()

	// update persists to director.json next to config.json
	lull := 9
	cap := 5
	dcfg.update(directorConfigPatch{LullSec: &lull, PerMapCap: &cap})

	if _, err := os.Stat(filepath.Join(dir, "director.json")); err != nil {
		t.Fatalf("director.json not written: %v", err)
	}

	// a fresh load reads it back
	loadDirectorConfig()
	got := dcfg.get()
	if got.LullSec != 9 || got.PerMapCap != 5 {
		t.Errorf("round-trip lost values: lull=%d cap=%d", got.LullSec, got.PerMapCap)
	}
}
