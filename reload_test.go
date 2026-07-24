package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestConfigChangesBucketsFields(t *testing.T) {
	base := Config{
		Token: "tok", Listen: "a", PipeAddr: "p", EtArgs: []string{"x"},
		DirMinSec: 120, SceneLive: "Live", ObsAddr: "o", PostConnectExec: []string{"a"},
	}

	// identical -> nothing changed
	if ch, nr := configChanges(base, base); len(ch) != 0 || len(nr) != 0 {
		t.Fatalf("identical configs must report no change, got changed=%v restart=%v", ch, nr)
	}

	// hot fields
	n := base
	n.DirMinSec = 90
	n.SceneLive = "L2"
	n.PostConnectExec = []string{"snd_restart"}
	ch, nr := configChanges(base, n)
	for _, k := range []string{"dir_min_sec", "scene_live", "post_connect_exec"} {
		if !containsStr(ch, k) {
			t.Errorf("%s should be in changed, got %v", k, ch)
		}
	}
	if len(nr) != 0 {
		t.Errorf("no restart expected, got %v", nr)
	}

	// restart fields (incl. slice et_args and resolution)
	n2 := base
	n2.Token = "new"
	n2.Listen = "b"
	n2.EtArgs = []string{"x", "y"}
	n2.Resolution = "720p"
	ch, nr = configChanges(base, n2)
	for _, k := range []string{"token", "listen", "et_args", "resolution"} {
		if !containsStr(nr, k) {
			t.Errorf("%s should need restart, got %v", k, nr)
		}
	}
	if len(ch) != 0 {
		t.Errorf("no hot change expected, got %v", ch)
	}
}

func TestApplyLiveConfigLeavesRestartFields(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg = Config{Token: "orig", Listen: "orig", PipeAddr: "orig", EtPath: "orig",
		DirMinSec: 120, SceneLive: "Live", ReplaySpeed: 0.4}
	n := Config{Token: "new", Listen: "new", PipeAddr: "new", EtPath: "new",
		DirMinSec: 90, SceneLive: "L2", ReplaySpeed: 0.2}
	applyLiveConfig(&n)
	// hot fields applied
	if cfg.DirMinSec != 90 || cfg.SceneLive != "L2" || cfg.ReplaySpeed != 0.2 {
		t.Errorf("hot fields not applied: %+v", cfg)
	}
	// restart fields untouched
	if cfg.Token != "orig" || cfg.Listen != "orig" || cfg.PipeAddr != "orig" || cfg.EtPath != "orig" {
		t.Errorf("restart fields must not change on a live reload: %+v", cfg)
	}
}

// end-to-end: reloadConfig reads the file, applies hot, and refuses restart-only
// fields while reporting them.
func TestReloadConfigRoundTrip(t *testing.T) {
	oldCfg, oldPath := cfg, configPath
	defer func() { cfg, configPath = oldCfg, oldPath }()

	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	cfg = Config{Token: strings.Repeat("x", 16), Listen: "0.0.0.0:8788", DirMinSec: 120, SceneLive: "Live"}

	js := `{"token":"xxxxxxxxxxxxxxxx","listen":"0.0.0.0:9999","dir_min_sec":90,"scene_live":"OnAir"}`
	if err := os.WriteFile(configPath, []byte(js), 0644); err != nil {
		t.Fatal(err)
	}
	changed, needsRestart, err := reloadConfig()
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if cfg.DirMinSec != 90 || cfg.SceneLive != "OnAir" {
		t.Errorf("hot fields not applied live: dir=%d scene=%s", cfg.DirMinSec, cfg.SceneLive)
	}
	if cfg.Listen != "0.0.0.0:8788" {
		t.Errorf("listen (restart-only) must not change live, got %s", cfg.Listen)
	}
	if !containsStr(changed, "dir_min_sec") || !containsStr(changed, "scene_live") {
		t.Errorf("changed should list the hot fields, got %v", changed)
	}
	if !containsStr(needsRestart, "listen") {
		t.Errorf("needs_restart should list listen, got %v", needsRestart)
	}
}

// a bad file is rejected and the running config is kept.
func TestReloadConfigRejectsBadFile(t *testing.T) {
	oldCfg, oldPath := cfg, configPath
	defer func() { cfg, configPath = oldCfg, oldPath }()

	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	cfg = Config{Token: strings.Repeat("x", 16), DirMinSec: 120}

	// invalid JSON
	os.WriteFile(configPath, []byte("{not json"), 0644)
	if _, _, err := reloadConfig(); err == nil {
		t.Error("invalid JSON should error")
	}
	if cfg.DirMinSec != 120 {
		t.Error("running config must be kept on a bad reload")
	}

	// valid JSON but token too short -> validation error, config kept
	os.WriteFile(configPath, []byte(`{"token":"short","dir_min_sec":5}`), 0644)
	if _, _, err := reloadConfig(); err == nil {
		t.Error("short token should fail validation")
	}
	if cfg.DirMinSec != 120 {
		t.Error("running config must be kept when validation fails")
	}
}
