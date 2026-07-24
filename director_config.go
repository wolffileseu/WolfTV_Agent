package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// DirectorConfig holds the auto-director settings that must be adjustable at
// runtime, without restarting the agent. The agent reads config.json only at
// startup; these live in a separate director.json (written next to config.json)
// so the hand-maintained config.json is never rewritten. The panel edits them
// through GET/POST /director/config and they apply live.
type DirectorConfig struct {
	// AutoReplay is the master switch for the automatic replay director
	// (Part 3). false = the agent still runs the live camera director and
	// records highlights, but never cuts a replay on its own.
	AutoReplay bool `json:"auto_replay"`

	// --- heat weighting for the live camera (Part 2) ---
	// The camera is steered toward the player with the most recent-kill "heat"
	// so it is already on whoever is most likely to make the next highlight.
	HeatWindowSec   int     `json:"heat_window_sec"`    // only kills newer than this count (default 30)
	HeatHalfLifeSec float64 `json:"heat_half_life_sec"` // recency weight halves every this many sec (default 8)
	HeatSpikeFactor float64 `json:"heat_spike_factor"`  // override the dir_min_sec floor when the leader's heat >= factor x the runner-up's (default 2.0)

	// --- auto-replay scheduling / guards (Part 3) ---
	LullSec            int `json:"lull_sec"`              // cut only after this many seconds with no kill/objective/mapchange (default 6)
	MinIntervalSec     int `json:"min_interval_sec"`      // minimum spacing between replays (default 180)
	MinHighlightAgeSec int `json:"min_highlight_age_sec"` // a highlight must be at least this old before use, so its demo data is on disk (default 60)
	PerMapCap          int `json:"per_map_cap"`           // hard cap on auto-replays per map (default 3)

	// --- playback window (runtime-adjustable copy of the replay window) ---
	PreSec  int     `json:"pre_sec"`  // seconds of demo before the highlight
	PostSec int     `json:"post_sec"` // seconds after
	Speed   float64 `json:"speed"`    // playback timescale, <1 = slow-mo
}

// directorConfigPatch is a partial update: only the non-nil fields are applied.
type directorConfigPatch struct {
	AutoReplay         *bool    `json:"auto_replay"`
	HeatWindowSec      *int     `json:"heat_window_sec"`
	HeatHalfLifeSec    *float64 `json:"heat_half_life_sec"`
	HeatSpikeFactor    *float64 `json:"heat_spike_factor"`
	LullSec            *int     `json:"lull_sec"`
	MinIntervalSec     *int     `json:"min_interval_sec"`
	MinHighlightAgeSec *int     `json:"min_highlight_age_sec"`
	PerMapCap          *int     `json:"per_map_cap"`
	PreSec             *int     `json:"pre_sec"`
	PostSec            *int     `json:"post_sec"`
	Speed              *float64 `json:"speed"`
}

// defaultDirectorConfig returns the built-in defaults. The playback window is
// seeded from config.json's replay window so the two agree out of the box.
func defaultDirectorConfig() DirectorConfig {
	pre, post, speed := cfg.ReplayPreSec, cfg.ReplayPostSec, cfg.ReplaySpeed
	if pre <= 0 {
		pre = 8
	}
	if post <= 0 {
		post = 5
	}
	if speed <= 0 {
		speed = 0.4
	}
	return DirectorConfig{
		AutoReplay:         false,
		HeatWindowSec:      30,
		HeatHalfLifeSec:    8,
		HeatSpikeFactor:    2.0,
		LullSec:            6,
		MinIntervalSec:     180,
		MinHighlightAgeSec: 60,
		PerMapCap:          3,
		PreSec:             pre,
		PostSec:            post,
		Speed:              speed,
	}
}

// applyDirectorPatch returns base with the patch's non-nil fields overlaid. Pure
// so the merge is unit-testable.
func applyDirectorPatch(base DirectorConfig, p directorConfigPatch) DirectorConfig {
	if p.AutoReplay != nil {
		base.AutoReplay = *p.AutoReplay
	}
	if p.HeatWindowSec != nil {
		base.HeatWindowSec = *p.HeatWindowSec
	}
	if p.HeatHalfLifeSec != nil {
		base.HeatHalfLifeSec = *p.HeatHalfLifeSec
	}
	if p.HeatSpikeFactor != nil {
		base.HeatSpikeFactor = *p.HeatSpikeFactor
	}
	if p.LullSec != nil {
		base.LullSec = *p.LullSec
	}
	if p.MinIntervalSec != nil {
		base.MinIntervalSec = *p.MinIntervalSec
	}
	if p.MinHighlightAgeSec != nil {
		base.MinHighlightAgeSec = *p.MinHighlightAgeSec
	}
	if p.PerMapCap != nil {
		base.PerMapCap = *p.PerMapCap
	}
	if p.PreSec != nil {
		base.PreSec = *p.PreSec
	}
	if p.PostSec != nil {
		base.PostSec = *p.PostSec
	}
	if p.Speed != nil {
		base.Speed = *p.Speed
	}
	return base
}

// clampDirectorConfig forces every field into a sane range. Pure and
// unit-tested: a panel typo (0, negative, absurd) must never wedge the director
// or divide-by-zero the heat math.
func clampDirectorConfig(c DirectorConfig) DirectorConfig {
	if c.HeatWindowSec < 5 {
		c.HeatWindowSec = 5
	}
	if c.HeatHalfLifeSec < 1 {
		c.HeatHalfLifeSec = 1
	}
	if c.HeatSpikeFactor < 1 {
		c.HeatSpikeFactor = 1
	}
	if c.LullSec < 1 {
		c.LullSec = 1
	}
	if c.MinIntervalSec < 0 {
		c.MinIntervalSec = 0
	}
	if c.MinHighlightAgeSec < 0 {
		c.MinHighlightAgeSec = 0
	}
	if c.PerMapCap < 0 {
		c.PerMapCap = 0
	}
	if c.PreSec < 0 {
		c.PreSec = 0
	}
	if c.PostSec < 0 {
		c.PostSec = 0
	}
	if c.Speed <= 0 {
		c.Speed = 0.4
	}
	return c
}

// directorConfigStore holds the live director config behind a mutex.
type directorConfigStore struct {
	mu  sync.Mutex
	cur DirectorConfig
}

var dcfg = &directorConfigStore{cur: DirectorConfig{HeatWindowSec: 30, HeatHalfLifeSec: 8,
	HeatSpikeFactor: 2, LullSec: 6, MinIntervalSec: 180, MinHighlightAgeSec: 60,
	PerMapCap: 3, PreSec: 8, PostSec: 5, Speed: 0.4}}

// directorConfigFile is director.json next to config.json.
func directorConfigFile() string {
	return filepath.Join(filepath.Dir(configPath), "director.json")
}

// loadDirectorConfig seeds defaults, overlays director.json if present, clamps,
// and stores the result. Called once at startup, after loadConfig.
func loadDirectorConfig() {
	c := defaultDirectorConfig()
	path := directorConfigFile()
	if data, err := os.ReadFile(path); err == nil {
		var p directorConfigPatch
		if err := json.Unmarshal(data, &p); err != nil {
			log.Printf("director config: %s is invalid (%v) -- using defaults", path, err)
		} else {
			c = applyDirectorPatch(c, p)
			log.Printf("director config: loaded %s", path)
		}
	}
	c = clampDirectorConfig(c)
	dcfg.mu.Lock()
	dcfg.cur = c
	dcfg.mu.Unlock()
}

// get returns a copy of the current director config.
func (s *directorConfigStore) get() DirectorConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// update overlays a patch, clamps, persists, and stores. Returns the new config.
func (s *directorConfigStore) update(p directorConfigPatch) DirectorConfig {
	s.mu.Lock()
	c := clampDirectorConfig(applyDirectorPatch(s.cur, p))
	s.cur = c
	s.mu.Unlock()
	if err := persistDirectorConfig(c); err != nil {
		log.Printf("director config: could not persist: %v", err)
	}
	return c
}

// persistDirectorConfig writes the config to director.json atomically (temp +
// rename) so a crash mid-write cannot leave a truncated file.
func persistDirectorConfig(c DirectorConfig) error {
	path := directorConfigFile()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// handleDirectorConfig serves the runtime director settings.
//
//	GET  /director/config -> the current settings
//	POST /director/config -> partial update {"lull_sec":8,...}, applied live and
//	                         persisted to director.json
func handleDirectorConfig(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, dcfg.get())
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "GET or POST"})
		return
	}
	var p directorConfigPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	c := dcfg.update(p)
	log.Printf("director config: updated via panel -> %+v", c)
	writeJSON(w, 200, c)
}
