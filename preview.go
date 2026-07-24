package main

import (
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/andreykaipov/goobs/api/requests/sources"
)

// GET /preview -> a JPEG snapshot of the current OBS program scene, so the panel
// can show what is actually on air. Auth-gated like everything else.
//
// Minimal implementation: it asks obs-websocket for a screenshot of the current
// program scene and streams the decoded JPEG back. A fuller preview.go may
// supersede this file.
func handlePreview(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	obs, err := obsClient()
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "obs not reachable: " + err.Error()})
		return
	}
	defer obs.Disconnect()

	scene := currentScene()
	if scene == "" {
		writeJSON(w, 503, map[string]string{"error": "no current OBS scene"})
		return
	}

	res, err := obs.Sources.GetSourceScreenshot(sources.NewGetSourceScreenshotParams().
		WithSourceName(scene).
		WithImageFormat("jpg").
		WithImageWidth(960))
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "screenshot failed: " + err.Error()})
		return
	}

	// ImageData is a data URI: "data:image/jpeg;base64,<...>"; keep the payload.
	data := res.ImageData
	if i := strings.Index(data, ","); i >= 0 {
		data = data[i+1:]
	}
	raw, derr := base64.StdEncoding.DecodeString(data)
	if derr != nil {
		writeJSON(w, 502, map[string]string{"error": "bad image data from obs"})
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}
