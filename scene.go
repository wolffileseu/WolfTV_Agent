package main

// OBS scene switching. Used standalone (operator picks a scene) and later by
// the replay director, which cuts to the replay scene and back to live.

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/andreykaipov/goobs/api/requests/scenes"
)

// setScene switches the OBS program scene by name.
func setScene(name string) error {
	c, err := obsClient()
	if err != nil {
		return err
	}
	defer c.Disconnect()
	_, err = c.Scenes.SetCurrentProgramScene(&scenes.SetCurrentProgramSceneParams{
		SceneName: &name,
	})
	return err
}

// currentScene returns the active program scene name ("" if unavailable).
func currentScene() string {
	c, err := obsClient()
	if err != nil {
		return ""
	}
	defer c.Disconnect()
	res, err := c.Scenes.GetCurrentProgramScene()
	if err != nil || res == nil {
		return ""
	}
	return res.SceneName
}

// resolveScene maps the logical names the panel sends ("live", "replay",
// "standby") to the configured OBS scene names. Any other value is passed
// through unchanged, so an operator can switch to an arbitrary scene.
func resolveScene(want string) string {
	switch want {
	case "live":
		return cfg.SceneLive
	case "replay":
		return cfg.SceneReplay
	case "standby":
		return cfg.SceneStandby
	}
	return want
}

// POST /scene  {"scene":"live"|"replay"|"standby"|"<exact OBS name>"}
// GET  /scene  -> {"scene":"<current>"}
func handleScene(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"scene": currentScene()})
		return
	}
	var req struct {
		Scene string `json:"scene"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Scene == "" {
		writeJSON(w, 400, map[string]string{"error": `body must be {"scene":"live|replay|standby|<name>"}`})
		return
	}
	name := resolveScene(req.Scene)
	if name == "" {
		writeJSON(w, 400, map[string]string{"error": "scene not configured: " + req.Scene})
		return
	}
	if err := setScene(name); err != nil {
		log.Println("scene:", err)
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	log.Println("scene ->", name)
	writeJSON(w, 200, map[string]any{"ok": true, "scene": name})
}
