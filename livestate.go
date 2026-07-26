package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// liveState is the small amount of per-run state a later (adopting) agent needs
// to keep watching -- and, crucially, to RELAUNCH -- the live ET it inherits.
// Everything else it needs (et_args, homepath, mod, resolution) is stable in
// config.json and recovered from there; only the runtime server pool, password
// and overlay change per deploy, so only those are persisted. Written next to
// config.json at every deploy, read back by adoptLiveET.
type liveState struct {
	Servers       []string `json:"servers"`
	Password      string   `json:"password"`
	CurrentServer string   `json:"current_server"`
	OverlayURL    string   `json:"overlay_url"`
}

func liveStateFile() string {
	return filepath.Join(filepath.Dir(configPath), "live-state.json")
}

// saveLiveState writes the state atomically (temp + rename), 0600 because it
// carries the server password.
func saveLiveState(s liveState) error {
	path := liveStateFile()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadLiveState reads the persisted state (error if absent/unreadable).
func loadLiveState() (liveState, error) {
	var s liveState
	data, err := os.ReadFile(liveStateFile())
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	return s, nil
}

// persistLiveStateLocked snapshots the live instance's runtime state and writes
// it in the background. Caller holds st.mu. Called on every deploy so an
// adopting agent always has a fresh, relaunch-capable picture.
func persistLiveStateLocked() {
	ls := liveState{
		Servers:       append([]string{}, st.servers...),
		Password:      st.password,
		CurrentServer: st.currentServer,
		OverlayURL:    st.overlayURL,
	}
	go func() {
		if err := saveLiveState(ls); err != nil {
			log.Println("livestate: save failed:", err)
		}
	}()
}
