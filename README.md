# WolfTV Agent

The control agent for **WolfTV** — an automated 24/7 spectator broadcast for
*Wolfenstein: Enemy Territory*. The agent runs on the streaming machine and
is the bridge between the [WolfTV panel](https://github.com/wolffileseu/wolftv-panel),
a patched ET client, and OBS.

```
Panel ──HTTP──► Agent ──TCP pipeline──► ET client ──► game server
                  │
                  └──WebSocket──► OBS
```

It's a single Go binary, cross-platform (Windows + Linux).

## What it does

- Launches and supervises the ET client, joins/switches servers seamlessly.
- Talks to the client over a line-based TCP pipeline: exec console commands,
  query the player list, follow a player, receive the live kill/objective
  feed and demo-segment markers.
- Drives OBS over obs-websocket: start/stop streaming, switch scenes.
- Monitors stream audio (via OBS levels) and auto-recovers on silence.
- Auto-director: rotates the spectator camera between active players.
- Exposes a small HTTP API the panel consumes, including a **/system**
  endpoint with live CPU / memory / disk / network stats.

## HTTP API

All endpoints require `Authorization: Bearer <token>`.

| Endpoint    | Method | Purpose                                      |
|-------------|--------|----------------------------------------------|
| `/start`    | POST   | launch ET + join a server, start streaming   |
| `/switch`   | POST   | seamless switch to another server            |
| `/stop`     | POST   | stop streaming (and optionally kill OBS)     |
| `/status`   | GET    | ET/OBS/pipeline/audio status + telemetry     |
| `/system`   | GET    | CPU / RAM / disk / network throughput        |
| `/events`   | GET    | live feed (kills, highlights, demo segments) |
| `/players`  | GET    | live roster                                  |
| `/follow`   | POST   | point the camera at a player slot            |
| `/exec`     | POST   | send a console command to the client         |
| `/log`      | GET    | tail the agent log                           |
| `/scene`    | GET/POST | read / switch the OBS program scene        |
| `/replay/segments` | GET | recorded demo segments + their highlights |
| `/replay`   | POST   | play back a demo segment around a highlight   |
| `/replay/stop`   | POST | abort a running replay, cut back to live  |
| `/replay/status` | GET | current replay phase/state                  |

## Two-instance model & replay cinema

The agent manages **N ET instances**, each with its own process handle, args,
`fs_homepath`, pipeline port and telemetry. In practice there are two:

| | **live** | **replay** |
|---|---|---|
| homepath | your live homepath | separate (`replay_homepath`, e.g. `C:\Stream\replayhome`) |
| ET install / pk3s | same install | same install |
| pipeline port | `pipe_addr` | `replay_pipe_addr` |
| streaming | yes | no |
| demo recording | yes (`cl_wtvDemo 1`) | no (forced `cl_wtvDemo 0`) |
| auto-director | yes | no (the replay orchestrator drives the camera) |
| lifetime | always on | started on demand, stopped after `replay_idle_stop_sec` idle |

The two instances **share no mutable state**. The live instance is `st`; the
replay instance is `rp`, each with its own mutex, pipeline goroutine and event
gating (`feedEvents` off for replay, so replayed demos never pollute the live
kill/highlight feed). Process kills are **by PID** — the old
`taskkill /IM <exe>` name sweep, which would have killed *both* ET processes,
is used only in single-instance mode.

**The live broadcast is never disrupted.** Every replay failure mode — instance
won't start, demo missing, pipeline dead, OBS switch fails, operator aborts —
funnels through one exit path that logs the error, stops touching the replay,
and makes sure OBS is back on the **live** scene. Nothing in the replay path
signals the live instance.

### Replay flow

1. **Ensure the replay instance is up.** Spawn it if needed (its own homepath,
   port, title, recording off) and wait for its pipeline hello, with a timeout.
2. **Load the demo.** The demo was recorded by the *live* instance into the live
   homepath. `demo <absolute-path>` is sent over the replay pipeline; ET opens
   an absolute path (with the `.dm_84` extension) directly, so the replay
   instance loads it despite running under a different homepath — no copy.
3. **Seek.** Set `timescale <replay_seek_timescale>` to fast-forward, then drop
   back to slow motion shortly before the window.
   *The WTV status protocol does not report the client's server time*, so
   elapsed demo time is estimated as `wall_time × timescale`. This biases toward
   **undershoot** (arriving a little early is safe; overshooting the highlight
   is not) — at a high timescale the client often can't render fast enough, so
   real demo time advances *slower* than the estimate. A fixed safety margin
   widens that bias.
4. **Cut OBS to the replay scene** (only *after* the timescale change succeeds,
   so a failed OBS switch never shows a fast-forward on the live scene).
5. **Play** `replay_pre_sec` before to `replay_post_sec` after the highlight at
   `replay_speed` (slow motion).
6. **Cut OBS back to the live scene.**
7. Leave the replay instance **warm** for `replay_idle_stop_sec`, then stop it
   (ET start is slow; a warm instance makes back-to-back replays fast).

Only **one replay at a time** — a second `POST /replay` returns `409`.

### Replay API

```
GET  /replay/segments
  -> {"segments":[{"file","map","seg_start_svtime","seg_end_svtime",
                   "highlights":[{"kind","player","score","svtime",
                                  "offset_ms","label"}]}]}   # newest first

POST /replay
  {"file":"wtv_supply_123456.dm_84","svtime":130000}        # highlight-based
  or {"file":"...","offset_ms":45000}                        # explicit offset
  optional overrides: "pre_sec","post_sec","speed"
  -> 200 {"ok":true,"file","offset_ms","eta_sec"}
  -> 409 if a replay is already running
  -> 400 on unknown file / offset outside the segment

POST /replay/stop      -> aborts, cuts back to the live scene immediately
GET  /replay/status    -> {"active","phase":"idle|starting|seeking|playing|
                           returning","file","offset_ms","started_at","instance_up"}
```

### Locating demos

The replay instance loads demos by **absolute path**, so the agent must know
where the live instance writes them: `<live_homepath>/<fs_game>/<replay_demo_dir>/`.
`live_homepath` and `fs_game` are auto-detected from `et_args`
(`+set fs_homepath …` / `+set fs_game …`); set them explicitly in the config if
they are not passed as ET args. `replay_demo_dir` must match the client's
`cl_wtvDemoDir` (default `wtvdemos`).

### Dry run

`dry_run: true` (or the `-dry-run` flag) exercises the **entire** replay
orchestration — instance start, `demo`, timescale seek, both OBS scene cuts,
and the playback waits — **without spawning ET or touching OBS**. Every step is
logged instead, so the flow can be validated on a dev machine:

```sh
./wolftv-agent -dry-run           # or set "dry_run": true in config.json
curl -H 'Authorization: Bearer <token>' -d '{"file":"wtv_supply_1.dm_84","offset_ms":45000}' \
     http://127.0.0.1:8788/replay
```

## Requirements

- Go 1.21+ (to build)
- OBS with obs-websocket (bundled in OBS 28+)
- A patched WolfTV / ET:Legacy client that speaks the WTV pipeline protocol
- On the streaming host: audio loopback (e.g. VB-CABLE on Windows) if you
  use the silence monitor

## Build

```sh
# native
go build -ldflags "-s -w" -o wolftv-agent .          # Linux
go build -ldflags "-s -w" -o wolftv-agent.exe .       # Windows

# cross-compile
GOOS=linux   GOARCH=amd64 go build -o wolftv-agent .
GOOS=windows GOARCH=amd64 go build -o wolftv-agent.exe .
```

Prebuilt binaries for both platforms are attached to each
[release](https://github.com/wolffileseu/wolftv-agent/releases).

## Configure & run

1. Copy `config.example.json` to `config.json` and fill in your values —
   at minimum the `token` (a long random string, shared with the panel),
   the paths to the ET client and OBS, and your obs-websocket password.
2. Run the binary in the same directory as `config.json`:
   ```sh
   ./wolftv-agent            # Linux
   wolftv-agent.exe          # Windows
   ```
3. Point the [panel](https://github.com/wolffileseu/wolftv-panel) at the
   agent's URL and token.

### Security

The agent's control port lets a caller drive your stream and run client
console commands. **Firewall the port to the panel host only** and use a long
random `token`. Never expose it to the open internet with the default token.

## Platform notes

Cross-platform behaviour is handled with Go build tags:

- `platform_windows.go` / `platform_linux.go` — process control
  (`taskkill` vs `pkill`) and default disk volume.
- The system monitor (`system.go`) uses
  [gopsutil](https://github.com/shirou/gopsutil), which is cross-platform.
- The audio **silence** monitor reads OBS levels, so it works on both. The
  Windows-only default-audio-**device** check (COM API) has a Linux no-op
  stub; PipeWire/PulseAudio control can be added later.

## License

MIT — see [LICENSE](LICENSE).

Part of the WolfTV project by [wahke](https://wahke.lu) /
[Wolffiles.eu](https://wolffiles.eu). The panel and the patched ET client
live in separate repositories.
