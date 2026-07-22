package main

import (
	"errors"
	"log"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/andreykaipov/goobs"
	"github.com/andreykaipov/goobs/api/requests/inputs"
)

func obsClient() (*goobs.Client, error) {
	if cfg.ObsPassword != "" {
		return goobs.New(cfg.ObsAddr, goobs.WithPassword(cfg.ObsPassword))
	}
	return goobs.New(cfg.ObsAddr)
}

func ensureOBS() (*goobs.Client, error) {
	if c, err := obsClient(); err == nil {
		return c, nil
	}
	if cfg.ObsPath == "" {
		return nil, errors.New("obs not reachable and obs_path not configured")
	}
	log.Println("obs: launching", cfg.ObsPath)
	cmd := exec.Command(cfg.ObsPath, "--disable-shutdown-check")
	cmd.Dir = filepath.Dir(cfg.ObsPath)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if c, err := obsClient(); err == nil {
			return c, nil
		}
	}
	return nil, errors.New("obs launched but websocket not reachable after 30s")
}

func setOverlayURL(url string) {
	if cfg.ObsBrowserInput == "" || url == "" {
		return
	}
	obs, err := obsClient()
	if err != nil {
		log.Println("overlay: obs not reachable:", err)
		return
	}
	defer obs.Disconnect()
	_, err = obs.Inputs.SetInputSettings(inputs.NewSetInputSettingsParams().
		WithInputName(cfg.ObsBrowserInput).
		WithInputSettings(map[string]any{"url": url}))
	if err != nil {
		log.Println("overlay: set url failed:", err)
	} else {
		log.Println("overlay: url ->", url)
	}
}

func obsStreamStatus() (obsUp, streaming bool) {
	obs, err := obsClient()
	if err != nil {
		return false, false
	}
	defer obs.Disconnect()
	s, err := obs.Stream.GetStreamStatus()
	if err != nil {
		return true, false
	}
	return true, s.OutputActive
}
