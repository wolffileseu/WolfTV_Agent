package main

import (
	"encoding/json"
	"log"
	"os"
)

type Config struct {
	Listen          string   `json:"listen"`            // "0.0.0.0:8788"
	Token           string   `json:"token"`             // bearer token (min 16)
	EtPath          string   `json:"et_path"`           // "C:\\ETLegacy\\etl.exe"
	EtExeName       string   `json:"et_exe_name"`       // "etl.exe"
	EtArgs          []string `json:"et_args"`           // extra args
	ObsPath         string   `json:"obs_path"`          // obs64.exe ("" = never launch)
	ObsAddr         string   `json:"obs_addr"`          // "localhost:4455"
	ObsPassword     string   `json:"obs_password"`      // "" = none
	ObsBrowserInput string   `json:"obs_browser_input"` // overlay source name, "" = skip
	KillObsOnStop   bool     `json:"kill_obs_on_stop"`

	// OBS scenes used for cutting the broadcast
	SceneLive    string `json:"scene_live"`    // main scene (default "Live")
	SceneReplay  string `json:"scene_replay"`  // replay scene, "" = replay cut disabled
	SceneStandby string `json:"scene_standby"` // standby/idle scene, "" = disabled

	// pipeline to the modified ET client (cl_wtvPort)
	PipeAddr string `json:"pipe_addr"` // "127.0.0.1:8790", "" = pipeline off

	// --- second (replay) ET instance -----------------------------------
	// A separate, non-streaming ET process used to play back recorded demo
	// segments around a highlight while the LIVE instance keeps streaming.
	// Everything here is inert unless replay_enabled is true; a failure in
	// the replay path never touches the live instance.
	ReplayEnabled  bool   `json:"replay_enabled"`
	ReplayPipeAddr string `json:"replay_pipe_addr"` // replay instance's cl_wtvPort pipe, e.g. "127.0.0.1:8791"
	// ReplayHomepath is DEPRECATED and ignored. The replay instance must run
	// under the SAME fs_homepath as the live instance -- a separate homepath
	// has none of the pk3s the demos need and playback fails with a checksum
	// error. The two are separated by ET profile (ReplayProfile) instead.
	ReplayHomepath string `json:"replay_homepath"`
	// ReplayProfile is the replay instance's cl_profile, so its profile.pid and
	// startup config exec do not collide with the live instance's in the shared
	// homepath (default "wolftv-replay").
	ReplayProfile       string  `json:"replay_profile"`
	ReplayIdleStopSec   int     `json:"replay_idle_stop_sec"`  // stop the warm replay instance after this idle time (default 300)
	ReplayPreSec        int     `json:"replay_pre_sec"`        // seconds of demo before the highlight (default 8)
	ReplayPostSec       int     `json:"replay_post_sec"`       // seconds of demo after the highlight (default 5)
	ReplaySpeed         float64 `json:"replay_speed"`          // playback timescale during the window, <1 = slow-mo (default 0.4)
	ReplaySeekTimescale int     `json:"replay_seek_timescale"` // fast-forward timescale while seeking (default 8)
	ReplayTitle         string  `json:"replay_title"`          // SDL/window title for the replay instance (default "WolfTV-Replay")

	// The replay instance plays demos recorded by the LIVE instance out of one
	// demo root under the shared fs_homepath, with a per-mod sub-directory
	// (<root>/<mod>/wtv_<map>_<svtime>.dm_84). The agent needs the live homepath
	// (auto-detected from et_args' +set fs_homepath) and the root's name; the
	// mod comes from the demo segment itself, never from config.
	LiveHomepath  string `json:"live_homepath"`   // "" = auto-detect from et_args
	ReplayDemoDir string `json:"replay_demo_dir"` // demo root under fs_homepath (matches cl_wtvDemoPath, default "wtvdemos")
	// FsGame is DEPRECATED and unused: demo paths are no longer fs_game-relative
	// and the mod to launch the replay instance with is reported per demo
	// segment. Kept only to warn when an old config still sets it.
	FsGame string `json:"fs_game"`

	// dry_run exercises the whole replay orchestration WITHOUT spawning ET or
	// touching OBS: every command/scene-cut/sleep is logged instead. Lets the
	// flow be tested on a dev machine. Also settable with the -dry-run flag.
	DryRun bool `json:"dry_run"`

	// director (brain lives here in Go)
	DirMinSec    int  `json:"dir_min_sec"`    // min sec between camera switches (120)
	DirMaxSec    int  `json:"dir_max_sec"`    // max sec (300)
	PreferHumans bool `json:"prefer_humans"`  // humans before bots
	SpecDelaySec int  `json:"spec_delay_sec"` // wait after map join before "team s" (5)

	// watchdog
	WatchName        string `json:"watch_name"`         // fallback check w/o pipeline
	WatchIntervalSec int    `json:"watch_interval_sec"` // 60
	WatchFailLimit   int    `json:"watch_fail_limit"`   // 3
	TeleTimeoutSec   int    `json:"tele_timeout_sec"`   // pipeline silence -> restart (30)
	SkipEmpty        bool   `json:"skip_empty"`

	LogFile string `json:"log_file"` // "" = console only, default "wolffiles-agent.log"

	// system monitor: volume reported by /system; "" = auto (C:\ or /)
	DiskPath string `json:"disk_path"`

	// console lines sent via pipeline each time the client enters a map
	// (after "team s"), e.g. ["snd_restart"] to re-bind the audio device
	PostConnectExec []string `json:"post_connect_exec"`

	// audio monitoring via OBS volume meter events
	AudioInput      string `json:"audio_input"`       // OBS input to watch, e.g. "CABLE Output"; "" = off
	AudioSilenceSec int    `json:"audio_silence_sec"` // silence longer than this = problem (default 30)
	// expected Windows default playback device (substring match),
	// e.g. "CABLE Input"; "" = check disabled
	WinExpectDefault string `json:"win_expect_default"`
	// send snd_restart automatically when silence is detected (cooldown 5min)
	AudioAutoFix bool `json:"audio_autofix"`
}

var cfg Config

const defaultConfig = `{
  "listen": "0.0.0.0:8788",
  "token": "CHANGE_ME_MIN_32_RANDOM_CHARS",

  "et_path": "C:\\ETLegacy\\etl.exe",
  "et_exe_name": "etl.exe",
  "et_args": [
    "+set", "r_mode", "-1",
    "+set", "r_customwidth", "1920",
    "+set", "r_customheight", "1080",
    "+set", "r_fullscreen", "0",
    "+set", "com_maxfps", "125",
    "+set", "s_volume", "0.5",
    "+set", "name", "^7Wolf^1TV ^8Bot",
    "+set", "cl_wtvPort", "8790",
    "+set", "cl_wtvTitle", "WolfTV-1"
  ],

  "obs_path": "C:\\Program Files\\obs-studio\\bin\\64bit\\obs64.exe",
  "obs_addr": "127.0.0.1:4455",
  "obs_password": "",
  "obs_browser_input": "Wolffiles Overlay",
  "kill_obs_on_stop": false,

  "scene_live": "Live",
  "scene_replay": "Replay",
  "scene_standby": "Standby",

  "pipe_addr": "127.0.0.1:8790",

  "replay_enabled": false,
  "replay_pipe_addr": "127.0.0.1:8791",
  "replay_profile": "wolftv-replay",
  "replay_idle_stop_sec": 300,
  "replay_pre_sec": 8,
  "replay_post_sec": 5,
  "replay_speed": 0.4,
  "replay_seek_timescale": 8,
  "replay_title": "WolfTV-Replay",
  "live_homepath": "",
  "replay_demo_dir": "wtvdemos",
  "dry_run": false,

  "dir_min_sec": 120,
  "dir_max_sec": 300,
  "prefer_humans": true,
  "spec_delay_sec": 5,
  "post_connect_exec": [],

  "watch_name": "WolfTV Bot",
  "watch_interval_sec": 60,
  "watch_fail_limit": 3,
  "tele_timeout_sec": 30,
  "skip_empty": true,

  "audio_input": "CABLE Output",
  "audio_silence_sec": 30,
  "audio_autofix": true,
  "win_expect_default": "CABLE Input",

  "disk_path": "",
  "log_file": "wolffiles-agent.log"
}
`

// configPath is where config.json was read from; director.json (the runtime,
// panel-editable director settings) is written next to it.
var configPath = "config.json"

func loadConfig() {
	path := "config.json"
	dryFlag := false
	for _, a := range os.Args[1:] {
		if a == "-dry-run" || a == "--dry-run" {
			dryFlag = true
			continue
		}
		path = a // first non-flag argument is the config path
	}
	configPath = path
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if werr := os.WriteFile(path, []byte(defaultConfig), 0644); werr != nil {
			log.Fatalf("config: %s fehlt und konnte nicht angelegt werden: %v", path, werr)
		}
		log.Printf("config: %s fehlte -- eine Vorlage wurde erstellt.", path)
		log.Printf("config: Bitte bearbeiten (mindestens: token, et_path, obs_password) und den Agent neu starten.")
		os.Exit(1)
	}
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("config parse: %v", err)
	}
	if len(cfg.Token) < 16 {
		log.Fatal("config: token missing or too short")
	}
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	if cfg.EtExeName == "" {
		cfg.EtExeName = "etl.exe"
	}
	if cfg.ObsAddr == "" {
		cfg.ObsAddr = "localhost:4455"
	}
	if cfg.Listen == "" {
		cfg.Listen = "0.0.0.0:8788"
	}
	// Live is the scene we always cut back to, so it needs a sane default.
	// Replay/Standby stay empty if unset -- /scene then reports them as
	// unconfigured instead of switching to a scene that doesn't exist.
	if cfg.SceneLive == "" {
		cfg.SceneLive = "Live"
	}
	def(&cfg.DirMinSec, 120)
	def(&cfg.DirMaxSec, 300)
	def(&cfg.SpecDelaySec, 5)
	def(&cfg.WatchIntervalSec, 60)
	def(&cfg.WatchFailLimit, 3)
	def(&cfg.TeleTimeoutSec, 30)
	if cfg.LogFile == "" {
		cfg.LogFile = "wolffiles-agent.log"
	}
	def(&cfg.AudioSilenceSec, 30)
	if cfg.DirMaxSec < cfg.DirMinSec {
		cfg.DirMaxSec = cfg.DirMinSec
	}

	// --- replay defaults ---
	if dryFlag {
		cfg.DryRun = true
	}
	def(&cfg.ReplayIdleStopSec, 300)
	def(&cfg.ReplayPreSec, 8)
	def(&cfg.ReplayPostSec, 5)
	def(&cfg.ReplaySeekTimescale, 8)
	if cfg.ReplaySpeed <= 0 {
		cfg.ReplaySpeed = 0.4
	}
	if cfg.ReplayTitle == "" {
		cfg.ReplayTitle = "WolfTV-Replay"
	}
	if cfg.ReplayProfile == "" {
		cfg.ReplayProfile = "wolftv-replay"
	}
	if cfg.ReplayDemoDir == "" {
		cfg.ReplayDemoDir = "wtvdemos"
	}
	// Auto-detect the live homepath from et_args unless set. The replay
	// instance runs under this same homepath -- it is the only place the pk3s
	// the demos need exist.
	if cfg.LiveHomepath == "" {
		cfg.LiveHomepath = argValue(cfg.EtArgs, "fs_homepath")
	}
	if cfg.ReplayHomepath != "" && cfg.ReplayHomepath != cfg.LiveHomepath {
		log.Printf("config: replay_homepath (%s) is obsolete and IGNORED -- the replay instance "+
			"runs under the live homepath (%s), otherwise it has none of the pk3s the demos need",
			cfg.ReplayHomepath, cfg.LiveHomepath)
	}
	cfg.ReplayHomepath = ""
	if cfg.FsGame != "" {
		log.Printf("config: fs_game (%s) is obsolete and IGNORED -- demos live in one flat "+
			"directory and each segment reports the mod that recorded it", cfg.FsGame)
		cfg.FsGame = ""
	}
}

// argValue returns the value following `+set <key>` (or a bare `<key>`) in an
// ET argument list, or "" if absent. Used to recover fs_homepath / fs_game.
func argValue(args []string, key string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key || args[i] == "+set" && i+2 < len(args) && args[i+1] == key {
			if args[i] == key {
				return args[i+1]
			}
			return args[i+2]
		}
	}
	return ""
}
