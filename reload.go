package main

import (
	"log"
	"net/http"
	"os"
)

// /reload re-reads config.json without a process restart. It applies what can
// safely take effect live and reports what changed but still needs a restart.
// If the new file fails to parse or validate, the running config is kept.

// equalStrs compares two string slices element-wise.
func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// configChanges compares old and new configs and buckets every changed key into
// "changed" (applied live) or "needsRestart" (requires a process or ET restart).
// Pure, so the classification is unit-tested. Both slices are non-nil so the
// JSON response is [] rather than null.
func configChanges(old, next Config) (changed, needsRestart []string) {
	changed = []string{}
	needsRestart = []string{}
	hot := func(name string, differs bool) {
		if differs {
			changed = append(changed, name)
		}
	}
	rst := func(name string, differs bool) {
		if differs {
			needsRestart = append(needsRestart, name)
		}
	}

	// --- needs a restart (bound at process start or only read when ET launches) ---
	rst("listen", old.Listen != next.Listen)
	rst("token", old.Token != next.Token)
	rst("pipe_addr", old.PipeAddr != next.PipeAddr)
	rst("replay_pipe_addr", old.ReplayPipeAddr != next.ReplayPipeAddr)
	rst("et_path", old.EtPath != next.EtPath)
	rst("et_exe_name", old.EtExeName != next.EtExeName)
	rst("et_args", !equalStrs(old.EtArgs, next.EtArgs))
	rst("live_homepath", old.LiveHomepath != next.LiveHomepath)
	rst("replay_enabled", old.ReplayEnabled != next.ReplayEnabled)
	rst("resolution", old.Resolution != next.Resolution) // only read at ET launch
	rst("log_file", old.LogFile != next.LogFile)         // log sink set at startup
	// Twitch client + goroutine are built at startup from these -> restart.
	rst("twitch_enabled", old.TwitchEnabled != next.TwitchEnabled)
	rst("twitch_client_id", old.TwitchClientID != next.TwitchClientID)
	rst("twitch_client_secret", old.TwitchClientSecret != next.TwitchClientSecret)
	rst("twitch_refresh_token", old.TwitchRefreshToken != next.TwitchRefreshToken)
	rst("twitch_broadcaster_id", old.TwitchBroadcasterID != next.TwitchBroadcasterID)

	// --- applies live (re-read by the goroutines that use them) ---
	hot("scene_live", old.SceneLive != next.SceneLive)
	hot("scene_replay", old.SceneReplay != next.SceneReplay)
	hot("scene_standby", old.SceneStandby != next.SceneStandby)
	hot("obs_addr", old.ObsAddr != next.ObsAddr)
	hot("obs_password", old.ObsPassword != next.ObsPassword)
	hot("obs_path", old.ObsPath != next.ObsPath)
	hot("obs_browser_input", old.ObsBrowserInput != next.ObsBrowserInput)
	hot("kill_obs_on_stop", old.KillObsOnStop != next.KillObsOnStop)
	hot("dir_min_sec", old.DirMinSec != next.DirMinSec)
	hot("dir_max_sec", old.DirMaxSec != next.DirMaxSec)
	hot("prefer_humans", old.PreferHumans != next.PreferHumans)
	hot("spec_delay_sec", old.SpecDelaySec != next.SpecDelaySec)
	hot("watch_name", old.WatchName != next.WatchName)
	hot("watch_interval_sec", old.WatchIntervalSec != next.WatchIntervalSec)
	hot("watch_fail_limit", old.WatchFailLimit != next.WatchFailLimit)
	hot("tele_timeout_sec", old.TeleTimeoutSec != next.TeleTimeoutSec)
	hot("skip_empty", old.SkipEmpty != next.SkipEmpty)
	hot("audio_input", old.AudioInput != next.AudioInput)
	hot("audio_silence_sec", old.AudioSilenceSec != next.AudioSilenceSec)
	hot("audio_autofix", old.AudioAutoFix != next.AudioAutoFix)
	hot("win_expect_default", old.WinExpectDefault != next.WinExpectDefault)
	hot("disk_path", old.DiskPath != next.DiskPath)
	hot("replay_pre_sec", old.ReplayPreSec != next.ReplayPreSec)
	hot("replay_post_sec", old.ReplayPostSec != next.ReplayPostSec)
	hot("replay_speed", old.ReplaySpeed != next.ReplaySpeed)
	hot("replay_seek_timescale", old.ReplaySeekTimescale != next.ReplaySeekTimescale)
	hot("replay_seek_mode", old.ReplaySeekMode != next.ReplaySeekMode)
	hot("replay_idle_stop_sec", old.ReplayIdleStopSec != next.ReplayIdleStopSec)
	hot("replay_profile", old.ReplayProfile != next.ReplayProfile)
	hot("replay_title", old.ReplayTitle != next.ReplayTitle)
	hot("replay_demo_dir", old.ReplayDemoDir != next.ReplayDemoDir)
	hot("post_connect_exec", !equalStrs(old.PostConnectExec, next.PostConnectExec))
	hot("dry_run", old.DryRun != next.DryRun)
	hot("twitch_title_template", old.TwitchTitleTemplate != next.TwitchTitleTemplate)
	return changed, needsRestart
}

// applyLiveConfig copies the hot-reloadable fields from n into the running cfg.
// The restart-only fields (listen/token/pipe addrs/et_*/live_homepath/etc.) are
// deliberately left at their running values so a live reload never half-applies
// something that needs a restart.
func applyLiveConfig(n *Config) {
	cfg.SceneLive = n.SceneLive
	cfg.SceneReplay = n.SceneReplay
	cfg.SceneStandby = n.SceneStandby
	cfg.ObsAddr = n.ObsAddr
	cfg.ObsPassword = n.ObsPassword
	cfg.ObsPath = n.ObsPath
	cfg.ObsBrowserInput = n.ObsBrowserInput
	cfg.KillObsOnStop = n.KillObsOnStop
	cfg.DirMinSec = n.DirMinSec
	cfg.DirMaxSec = n.DirMaxSec
	cfg.PreferHumans = n.PreferHumans
	cfg.SpecDelaySec = n.SpecDelaySec
	cfg.WatchName = n.WatchName
	cfg.WatchIntervalSec = n.WatchIntervalSec
	cfg.WatchFailLimit = n.WatchFailLimit
	cfg.TeleTimeoutSec = n.TeleTimeoutSec
	cfg.SkipEmpty = n.SkipEmpty
	cfg.AudioInput = n.AudioInput
	cfg.AudioSilenceSec = n.AudioSilenceSec
	cfg.AudioAutoFix = n.AudioAutoFix
	cfg.WinExpectDefault = n.WinExpectDefault
	cfg.DiskPath = n.DiskPath
	cfg.ReplayPreSec = n.ReplayPreSec
	cfg.ReplayPostSec = n.ReplayPostSec
	cfg.ReplaySpeed = n.ReplaySpeed
	cfg.ReplaySeekTimescale = n.ReplaySeekTimescale
	cfg.ReplaySeekMode = n.ReplaySeekMode
	cfg.ReplayIdleStopSec = n.ReplayIdleStopSec
	cfg.ReplayProfile = n.ReplayProfile
	cfg.ReplayTitle = n.ReplayTitle
	cfg.ReplayDemoDir = n.ReplayDemoDir
	cfg.PostConnectExec = n.PostConnectExec
	cfg.DryRun = n.DryRun
	cfg.TwitchTitleTemplate = n.TwitchTitleTemplate

	// side-effect: the disk monitor captured its path at startup, so nudge it.
	if sysmon != nil {
		sysmon.setDiskPath(n.DiskPath)
	}
}

// reloadConfig re-reads config.json, and on success applies the live fields and
// returns what changed. On any parse/validate error the running config is kept
// untouched. Serialised with cfgMu so two reloads cannot interleave.
func reloadConfig() (changed, needsRestart []string, err error) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	data, rerr := os.ReadFile(configPath)
	if rerr != nil {
		return nil, nil, rerr
	}
	next, perr := parseConfigBytes(data, false) // no deprecation spam on reload
	if perr != nil {
		return nil, nil, perr
	}
	changed, needsRestart = configChanges(cfg, next)
	applyLiveConfig(&next)
	return changed, needsRestart, nil
}

// POST /reload -> re-read config.json, apply live, report the split.
func handleReload(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	changed, needsRestart, err := reloadConfig()
	if err != nil {
		// keep the running config; never leave the agent half-configured
		writeJSON(w, 400, map[string]any{"error": "config not applied (kept running config): " + err.Error()})
		return
	}
	log.Printf("reload: applied=%v needs_restart=%v", changed, needsRestart)
	writeJSON(w, 200, map[string]any{"ok": true, "changed": changed, "needs_restart": needsRestart})
}
