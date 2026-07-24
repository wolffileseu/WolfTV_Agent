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
| `/replay/segments` | GET | recorded demo segments + their highlights (with `replayable`) |
| `/replay`   | POST   | play back a demo segment around a highlight   |
| `/replay/stop`   | POST | abort a running replay, cut back to live  |
| `/replay/status` | GET | current replay phase/state                  |
| `/director/config` | GET/POST | read / live-update the auto-director settings |
| `/director/status` | GET | what the auto-director is currently thinking |

## Two-instance model & replay cinema

The agent manages **N ET instances**, each with its own process handle, args,
`fs_homepath`, pipeline port and telemetry. In practice there are two:

| | **live** | **replay** |
|---|---|---|
| homepath | your live homepath | **the same one** (see below) |
| ET profile | yours | `replay_profile` (default `wolftv-replay`) |
| `fs_game` | the server's mod | **the mod that recorded the demo** |
| ET install / pk3s | same install | same install |
| pipeline port | `pipe_addr` | `replay_pipe_addr` |
| streaming | yes | no |
| demo recording | yes (`cl_wtvDemo 1`) | no (forced `cl_wtvDemo 0`) |
| audio device | yes | no (forced `s_initsound 0`) |
| auto-director | yes | no (the replay orchestrator drives the camera) |
| lifetime | always on | started on demand, stopped after `replay_idle_stop_sec` idle |

**The replay instance shares the live `fs_homepath`.** It is not an oversight:
a demo only plays if the client has the pk3s it references — the maps and the
mod — and those were downloaded into the *live* homepath over months of
streaming. A separate, empty homepath fails playback with a checksum error, so
`replay_homepath` is gone; the instances are separated by ET profile instead.

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

1. **Ensure the replay instance is up *and running the demo's mod*.** Spawn it
   if needed (shared homepath, own profile, port, title, recording off) and wait
   for its pipeline hello, with a timeout. A warm instance is **never reused
   across mods** — a demo is only playable by the mod that recorded it, so a
   request for a different mod restarts the instance with the right `fs_game`.
2. **Load the demo** with `wtvdemo <mod>/<file>` over the replay pipeline — the
   segment's path relative to the client's demo root (`cl_wtvDemoPath`),
   bypassing the `fs_game`-relative lookup that plain `demo` does.
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
  -> {"segments":[{"file","path","map","mod","mod_source","seg_start_svtime",
                   "seg_end_svtime",
                   "highlights":[{"kind","player","score","svtime",
                                  "offset_ms","label"}]}]}   # newest first
     # path = "<mod>/<file>", relative to the client's demo root
     # mod  = the fs_game needed to play the segment back;
     # mod_source = field (client-reported) | directory (from path) | unknown
     # mod "" / unknown -> that segment cannot be replayed

POST /replay
  {"file":"wtv_supply_123456.dm_84","svtime":130000}         # highlight-based
  or {"file":"...","offset_ms":45000}                        # explicit offset
  optional overrides: "pre_sec","post_sec","speed"
  -> 200 {"ok":true,"file","path","mod","mod_source","instance_action":
          "reuse|start|restart","offset_ms","eta_sec"}
  -> 409 if a replay is already running
  -> 400 on unknown file / offset outside the segment / undeterminable mod

POST /replay/stop      -> aborts, cuts back to the live scene immediately
GET  /replay/status    -> {"active","phase":"idle|starting|loading|seeking|
                           prepared|playing|returning","file","mod","offset_ms",
                           "started_at","instance_up","instance_mod"}
```

### Automatic replay director

A demo records only what the live camera was following, so **a highlight can be
replayed only if the camera was on the player who made it.** Every highlight in
`/events` and `/replay/segments` therefore carries `replayable` (plus `followed`
/ `followed_slot`); the panel greys out the rest, and the auto-director never
picks them.

- **Camera by heat.** The live camera is steered toward the player with the most
  recent-kill *heat* (each kill weighted `0.5^(age/half-life)`), so it is already
  on whoever is most likely to make the next multikill. Humans break ties;
  `dir_min_sec` is the floor, but a heat spike cuts early.
- **Cut on a lull.** Like a sports broadcast, the director replays during a quiet
  moment (no kills/objectives for `lull_sec`), never mid map change, never while
  the live pipeline is down, spaced by `min_interval_sec` and capped per map.
- **Prepared ahead of time.** As soon as a good candidate exists, the warm replay
  instance loads the demo, seeks to the window and **holds**, so the lull cut is
  instant instead of paying the ~40s cold cost.

It is **off by default** (`auto_replay:false`) and the manual `/replay` path is
unaffected by it.

```
GET  /director/config  -> the current settings (see director.json below)
POST /director/config  {"lull_sec":8,"auto_replay":true, ...}   # partial, live
  -> applies immediately and persists to director.json

GET  /director/status
  -> {"auto_enabled",
      "candidates":[{"file","player","score","age_sec","offset_ms",
                     "eligible",...}],          # ranked best-first
      "last_replay_at","next_eligible_at","quiet_for_sec","lull_sec",
      "map_replays","per_map_cap",
      "prepared":{"file","mod","phase","held","holding_for_sec"}}
```

**Runtime settings live in `director.json`**, written next to `config.json`
(never rewriting the hand-maintained `config.json`). It is created on first
`POST /director/config` and reloaded at startup:

```
{ "auto_replay": false,
  "heat_window_sec": 30, "heat_half_life_sec": 8, "heat_spike_factor": 2.0,
  "lull_sec": 6, "min_interval_sec": 180, "min_highlight_age_sec": 60,
  "per_map_cap": 3, "pre_sec": 8, "post_sec": 5, "speed": 0.4 }
```

> **Hold caveat (needs a live check).** Preparation holds the seeked demo with
> `timescale 0`. Whether ET truly freezes the demo parse there (vs. drifting)
> was not verifiable on the dev box. If a prepared replay starts late, that is
> why — disable `auto_replay`; the manual path (which seeks and plays in one go)
> is unaffected. `replay_seek_mode:"fastforward"` switches the seek to the
> client's parse-level `fastforward` command (accurate, near-instant, but also
> pending a live A/B; default `timescale`).

### Locating demos, and the mod

The client writes all demos into one root under `fs_homepath`
(`cl_wtvDemoPath`, mirrored by `replay_demo_dir`, default `wtvdemos`) — no
longer under `<fs_game>/` — with **one sub-directory per mod**:

```
<live_homepath>/<replay_demo_dir>/<mod>/wtv_<map>_<svtime>.dm_84
e.g.  wtvdemos/silent/wtv_axislab_final_45318300.dm_84
```

`live_homepath` is auto-detected from `et_args` (`+set fs_homepath …`); set it
explicitly if it is not passed as an ET arg. The agent only uses that path to
check the file is still on disk; the client resolves the demo itself from the
`<mod>/<file>` it is handed.

A demo can only be played back by **the mod that recorded it**. The agent takes
that mod, in order, from:

1. the `mod` field on the demo-segment pipeline message (the client reports it),
2. the mod **directory** of the reported `path` (`silent/wtv_…`),
3. nothing — the segment is reported with `mod_source: "unknown"` and `POST
   /replay` rejects it with a clear error. That is what happens to demos
   recorded before the client moved to per-mod directories: they are not
   playable through this path.

Nothing is parsed out of the filename. A mod name (`no_quarter`) and a map name
(`etl_sp_delivery`) can both contain underscores, so `wtv_<mod>_<map>_<svtime>`
could not be split back apart unambiguously — which is exactly why the client
puts the mod in a path segment instead.

There is no `fs_game` config field any more — it is per demo, not per install.

### Sharing one homepath safely

Two ET processes in one homepath can collide. What the replay instance does
about it, and what is left for you:

| shared thing | guard |
|---|---|
| `profiles/<x>/profile.pid` | `+set cl_profile <replay_profile>` **and** an explicit `+set com_pidfile profiles/<replay_profile>/profile.pid` |
| audio device | `+set s_initsound 0` — no second process fighting for it |
| `etl.db` (SQLite) | `+set db_mode 1` (in-memory) — the shared DB file is never opened |
| `etconsole.log` | `+set logfile 0` — ET opens that file **truncating**, which would eat the live instance's log |
| demo files | `+set cl_wtvDemo 0` — the replay instance never records |
| downloads | the replay instance never connects (`+connect` is stripped), so it never downloads. The live instance's demo-ring deletion can fail on a demo the replay instance has open — the file simply survives one rotation |
| `etconfig.cfg` | **not fully solvable from the agent** — see below |

`cl_profile` does *not* isolate the config write. It is `CVAR_ROM`, and
`Cvar_Get` force-resets a ROM cvar that the user set on the command line
(`cvar.c`, `CVAR_USER_CREATED` + `CVAR_ROM` → latched to the engine default),
so by the time `CL_Init` has run `cl_profile` is empty again. It still works for
the *startup* config exec and for `com_pidfile`, because both are read before
`CL_Init` — but `Com_WriteConfiguration` runs from `Com_Frame` whenever an
archived cvar changes, and by then it writes plain
`<fs_homepath>/<fs_game>/etconfig.cfg` for **both** instances.

The practical guard is on the live side: **pin every cvar the live instance
depends on in `et_args`**. Command-line `+set` is applied *after* the config
file is exec'd, so what the replay instance persists cannot change live
behaviour. The agent logs which of the cvars it forces are not pinned in
`et_args` when replay is enabled. (A complete fix belongs in the client: force
`cl_profile` back after `CL_Init` when `cl_wtvPort` is set.)

### Dry run

`dry_run: true` (or the `-dry-run` flag) exercises the **entire** replay
orchestration — mod resolution, instance start, `wtvdemo`, timescale seek, both
OBS scene cuts, and the playback waits — **without spawning ET or touching
OBS**. Every step is logged instead, including the resolved path, the mod, the
on-disk location, and whether the warm instance would have to be restarted:

```sh
./wolftv-agent -dry-run           # or set "dry_run": true in config.json
curl -H 'Authorization: Bearer <token>' \
     -d '{"file":"wtv_supply_1.dm_84","offset_ms":45000}' \
     http://127.0.0.1:8788/replay
# replay: accepted silent/wtv_supply_1.dm_84 (mod silent via field, instance: start) ...
# replay: [dry-run] demo "silent/wtv_supply_1.dm_84" (mod "silent", on disk
#         C:\Stream\livehome\wtvdemos\silent\wtv_supply_1.dm_84) -- would start the replay instance
# replay: exec "wtvdemo silent/wtv_supply_1.dm_84"
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
