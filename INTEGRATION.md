# WolfTV Agent — integrating the cross-platform + system-monitor changes

These are the exact edits to fold the new files into your existing agent
(the one on the Windows box with main.go, director.go, config.go, obs.go,
win_audio.go, pipeline.go, eventfeed.go, q3.go …).

New files to drop in (already written):
- `system.go`            — the gopsutil sampler + SystemStats
- `system_handler.go`    — the /system HTTP handler
- `platform_windows.go`  — Windows killProcessByName / defaultDiskPath (build-tagged)
- `platform_linux.go`    — Linux killProcessByName / defaultDiskPath (build-tagged)

## 1. Replace the two hardcoded `taskkill` calls

**director.go** (was around line 73):
```go
// OLD:
_ = exec.Command("taskkill", "/IM", cfg.EtExeName, "/F").Run()
// NEW:
_ = killProcessByName(cfg.EtExeName)
```

**main.go** (handleStop, was around line 209):
```go
// OLD:
_ = exec.Command("taskkill", "/IM", "obs64.exe", "/F").Run()
// NEW:
_ = killProcessByName("obs64.exe")
```

After this, `exec` may be unused in main.go — remove it from the imports if
the compiler complains (director.go still uses exec.Command for launching ET,
which is fine cross-platform).

## 2. Register the /system route

**main.go**, next to the other `http.HandleFunc` lines:
```go
http.HandleFunc("/system", handleSystem)
```

## 3. Start the monitor at boot

**main.go**, in `main()` after config is loaded and before/around where the
HTTP server starts:
```go
startSysMonitor(cfg.DiskPath) // cfg.DiskPath may be "" → sane per-OS default
```

## 4. Add one optional config field

**config.go**, in the Config struct:
```go
DiskPath string `json:"disk_path"` // volume to report in /system; "" = auto (C:\ or /)
```
No default needed — empty string auto-selects per platform.

## 5. (Optional) report platform in /status

If you want it visible, add to the /status JSON in handleStatus:
```go
"platform": platformName,
```

## 6. win_audio.go — make it Windows-only

Your `win_audio.go` uses the Windows COM API. Add a build tag at the very top
so Linux builds don't try to compile it:
```go
//go:build windows

package main
// … existing contents …
```
Then add a tiny Linux stub so the Linux build still has the same functions.
Create `audio_linux.go`:
```go
//go:build linux

package main

// Linux audio device check stub. PipeWire/PulseAudio control can be added
// later (pactl). For now the default-device guard is a no-op on Linux so the
// silence monitor (which reads OBS levels) still works cross-platform.
func ensureAudioDevice(_ string) (bool, error) { return true, nil }
```
Match the real function name/signature from your win_audio.go. If it exposes
more than one function, stub each. The OBS-level silence monitor in obs.go is
already cross-platform (it talks to obs-websocket, not the OS), so only the
default-device COM helper needs a Linux stub.

## 7. Build

Windows (on the Windows box, as before):
```
go build -ldflags "-s -w" -o wolftv-agent.exe .
```

Linux (on web01 or any Linux box):
```
go build -ldflags "-s -w" -o wolftv-agent .
```

Cross-compile from one OS to the other (no target machine needed):
```
GOOS=linux   GOARCH=amd64 go build -o wolftv-agent .
GOOS=windows GOARCH=amd64 go build -o wolftv-agent.exe .
```

## 8. Pull gopsutil

```
go get github.com/shirou/gopsutil/v3@latest
go mod tidy
```

That's it. `/system` starts returning live CPU/RAM/disk/net immediately, and
the panel dashboard's System card appears on its own once the endpoint
responds.
