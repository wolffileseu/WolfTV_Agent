package main

import (
	"fmt"
	"log"
	"math/rand"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var rng = rand.New(rand.NewSource(time.Now().UnixNano()))

/* ---------------- ET process ---------------- */

// spawn starts an ET process with the given args and tracks its lifecycle in
// the instance. Shared by the live and replay instances. Caller must hold in.mu.
func (in *instance) spawn(args []string) error {
	cmd := exec.Command(cfg.EtPath, args...)
	cmd.Dir = filepath.Dir(cfg.EtPath)
	if err := cmd.Start(); err != nil {
		return err
	}
	in.etCmd = cmd
	in.startedAt = time.Now()
	done := make(chan struct{})
	in.etDone = done
	go func() {
		_ = cmd.Wait()
		close(done)
		in.mu.Lock()
		if in.etCmd == cmd {
			in.etCmd = nil
			log.Printf("et[%s]: process exited", in.name)
		}
		in.mu.Unlock()
	}()
	return nil
}

func startET(server, password string) error {
	args := append([]string{}, cfg.EtArgs...)
	if password != "" {
		args = append(args, "+password", password)
	}
	args = append(args, "+connect", server)
	if err := st.spawn(args); err != nil {
		return err
	}
	st.currentServer = server
	st.nameFails = 0
	resetDirectorLocked()
	return nil
}

/* kill: graceful first (pipeline "quit", up to 5s), hard kill as fallback.
 * Avoids ET:Legacy's "crashed last time" dialog which would block the next
 * automated start. Caller must hold in.mu; the lock is briefly released while
 * waiting for the clean exit.
 *
 * MULTI-INSTANCE SAFETY: the kill is by PID only. The old name-based
 * `taskkill /IM <exe>` sweep would kill BOTH ET processes (live and replay
 * share the executable name), so it is used only in single-instance mode. */
func (in *instance) kill() {
	if in.etCmd != nil {
		done := in.etDone
		if in.pipeUp && in.pipeExecLocked("quit") {
			log.Printf("et[%s]: sent quit, waiting for clean exit", in.name)
			if done != nil {
				in.mu.Unlock()
				select {
				case <-done:
					log.Printf("et[%s]: exited cleanly", in.name)
				case <-time.After(5 * time.Second):
					log.Printf("et[%s]: quit timeout -> hard kill", in.name)
				}
				in.mu.Lock()
			}
		}
	}
	if in.etCmd != nil && in.etCmd.Process != nil {
		_ = in.etCmd.Process.Kill()
		in.etCmd = nil
	}
	if !cfg.ReplayEnabled {
		_ = killProcessByName(cfg.EtExeName)
	}
}

/* deploy: switch to the best server from the list.
 * Uses the pipeline (connect command, no restart) when available,
 * falls back to kill+start. Caller must hold st.mu. */
func deploy(servers []string, password, overlayURL string) (string, error) {
	target := pickServer(servers)
	if target == "" {
		return "", errStr("no server from list reachable")
	}
	st.servers = servers
	st.password = password
	if overlayURL != "" {
		st.overlayURL = overlayURL
	}

	if st.etCmd != nil && st.pipeUp {
		line := "connect " + target
		if password != "" {
			line = "password \"" + password + "\";" + line
		}
		if st.pipeExecLocked(line) {
			log.Println("deploy: pipeline connect ->", target)
			st.currentServer = target
			st.tele.State = "connecting" // expected brief disconnect follows
			st.discSince = time.Time{}
			resetDirectorLocked()
			go setOverlayURL(st.overlayURL)
			return target, nil
		}
	}

	st.kill()
	time.Sleep(1 * time.Second)
	log.Println("deploy: (re)starting ET ->", target)
	if err := startET(target, password); err != nil {
		return "", err
	}
	go setOverlayURL(st.overlayURL)
	return target, nil
}

type errStr string

func (e errStr) Error() string { return string(e) }

/* ---------------- director (the brain) ---------------- */

func resetDirectorLocked() {
	st.curTargetSlot = -1
	st.specSent = false
	st.activeSince = time.Time{}
	st.nextSwitch = time.Time{}
	st.curTarget = ""
}

func scheduleNextLocked() {
	d := cfg.DirMinSec + rng.Intn(cfg.DirMaxSec-cfg.DirMinSec+1)
	st.nextSwitch = time.Now().Add(time.Duration(d) * time.Second)
}

// directorLoop drives spectating via the pipeline. 1s tick.
func directorLoop() {
	for {
		time.Sleep(1 * time.Second)
		st.mu.Lock()
		if !st.desired || !st.pipeUp || st.etCmd == nil {
			st.mu.Unlock()
			continue
		}
		if st.tele.State != "active" {
			resetDirectorLocked()
			st.mu.Unlock()
			continue
		}
		if st.activeSince.IsZero() {
			st.activeSince = time.Now()
		}
		// 1) go spectator once per map
		if !st.specSent {
			if time.Since(st.activeSince) > time.Duration(cfg.SpecDelaySec)*time.Second {
				st.pipeExecLocked("team s")
				for _, line := range cfg.PostConnectExec {
					st.pipeExecLocked(line)
					log.Println("director: post-connect exec:", line)
				}
				st.specSent = true
				st.nextSwitch = time.Now().Add(3 * time.Second)
				log.Println("director: team spectator")
			}
			st.mu.Unlock()
			continue
		}
		// free-cam detection: client says "not following" although we set
		// a target (map change, target left, speclock...) -> re-pick soon.
		// Suppressed during a manual hold so a followed player's death/
		// respawn free-cam doesn't bounce the camera away.
		if st.tele.FollowSlot != nil && st.curTargetSlot >= 0 &&
			time.Now().After(st.manualUntil) {
			if *st.tele.FollowSlot < 0 {
				if st.freeSince.IsZero() {
					st.freeSince = time.Now()
				} else if time.Since(st.freeSince) > 10*time.Second {
					log.Println("director: free cam detected -> picking new target")
					st.nextSwitch = time.Now()
					st.freeSince = time.Time{}
				}
			} else {
				st.freeSince = time.Time{}
			}
		}

		// spike override (Part 2): if a player is far hotter than everyone else
		// and the camera is not on them, cut early -- that is where the next
		// multikill is coming from. This overrides the dir_min_sec floor, but
		// only every spikeMinIntervalSec so a busy fight can't make it twitch.
		dc := dcfg.get()
		if leader, top, second := leaderByHeat(
			feed.heatBySlot(time.Now().UnixMilli(), dc.HeatWindowSec, dc.HeatHalfLifeSec)); leader >= 0 &&
			leader != st.curTargetSlot && top >= heatSpikeFloor && top >= dc.HeatSpikeFactor*second &&
			time.Since(st.lastSwitch) >= spikeMinIntervalSec*time.Second &&
			time.Now().After(st.manualUntil) {
			if st.nextSwitch.After(time.Now()) {
				log.Printf("director: heat spike slot %d (%.2f vs %.2f) -> early switch", leader, top, second)
			}
			st.nextSwitch = time.Now()
		}

		// 2) camera switch due?
		if time.Now().Before(st.nextSwitch) {
			st.mu.Unlock()
			continue
		}
		server := st.currentServer
		current := st.curTarget
		curSlot := st.curTargetSlot
		st.mu.Unlock()

		// fresh heat for the pick itself (the demo camera should already be on
		// the hottest player when the next highlight lands).
		heat := feed.heatBySlot(time.Now().UnixMilli(), dc.HeatWindowSec, dc.HeatHalfLifeSec)

		// preferred: exact player/team info straight from the client
		if players, qok := queryPlayers(2 * time.Second); qok {
			slot, raw, name, ping, found := pickFromQuery(players, cfg.WatchName, curSlot, server, heat)
			st.mu.Lock()
			if found {
				st.curTarget = name
				st.curTargetSlot = slot
				st.lastSwitch = time.Now()
				// silEnT (and most mods) match "follow <slot>", NOT the name.
				// Slots are reliable since the client-side off-by-N fix.
				st.pipeExecLocked(fmt.Sprintf("follow %d", slot))
				log.Printf("director: follow slot %d %q (ping %d, heat %.2f)", slot, name, ping, heat[slot])
				_ = raw
			} else {
				log.Println("director: nobody playing (specs only) -> free cam")
			}
			scheduleNextLocked()
			st.mu.Unlock()
			continue
		}

		// fallback: getstatus-based guess (client without query capability)
		raw, ping, ok := chooseTarget(server, cfg.WatchName, current)

		st.mu.Lock()
		if ok {
			st.curTarget = cleanName(raw)
			st.lastSwitch = time.Now()
			st.pipeExecLocked(`follow "` + strings.ReplaceAll(raw, `"`, "") + `"`)
			log.Printf("director: follow %q (ping %d)", st.curTarget, ping)
		} else {
			st.pipeExecLocked("follownext")
			log.Println("director: follownext (status unavailable/empty)")
		}
		scheduleNextLocked()
		st.mu.Unlock()
	}
}

/* ---------------- watchdog ---------------- */

func watchdog() {
	for {
		time.Sleep(time.Duration(cfg.WatchIntervalSec) * time.Second)
		st.mu.Lock()
		if !st.desired {
			st.mu.Unlock()
			continue
		}
		// 1) process dead
		if st.etCmd == nil {
			log.Println("watchdog: et dead -> relaunch")
			if _, err := deploy(st.servers, st.password, ""); err != nil {
				log.Println("watchdog: relaunch failed:", err)
			}
			st.mu.Unlock()
			continue
		}
		// 2) pipeline silent too long (client frozen)
		if cfg.PipeAddr != "" && st.pipeUp &&
			time.Since(st.lastEvent) > time.Duration(cfg.TeleTimeoutSec)*time.Second {
			log.Println("watchdog: pipeline silent -> restart et")
			st.kill()
			if _, err := deploy(st.servers, st.password, ""); err != nil {
				log.Println("watchdog: restart failed:", err)
			}
			st.mu.Unlock()
			continue
		}
		// 3) telemetry says disconnected and it persisted (client reconnects
		//    on its own during a pipeline server switch -- give it 20s)
		if st.pipeUp && st.tele.State == "disconnected" &&
			!st.discSince.IsZero() && time.Since(st.discSince) > 20*time.Second {
			log.Println("watchdog: client disconnected -> redeploy")
			if _, err := deploy(st.servers, st.password, ""); err != nil {
				log.Println("watchdog: redeploy failed:", err)
			}
			st.mu.Unlock()
			continue
		}
		server := st.currentServer
		pipeUp := st.pipeUp
		st.mu.Unlock()

		// 4+5) ONE status query (with retries against flood protection) used
		// for both the reachability check and the no-pipeline name check.
		var players []q3Player
		alive := false
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				time.Sleep(1500 * time.Millisecond)
			}
			if p, ok := q3GetStatus(server); ok {
				players, alive = p, true
				break
			}
		}
		if !alive {
			log.Println("watchdog: server unreachable -> switching")
			st.mu.Lock()
			if _, err := deploy(st.servers, st.password, ""); err != nil {
				log.Println("watchdog: switch failed:", err)
			}
			st.mu.Unlock()
			continue
		}
		// legacy fallback without pipeline: our name in THIS player list?
		if !pipeUp && cfg.WatchName != "" {
			found := false
			want := strings.ToLower(cleanName(cfg.WatchName))
			for _, p := range players {
				if strings.Contains(strings.ToLower(cleanName(p.Name)), want) {
					found = true
					break
				}
			}
			st.mu.Lock()
			if found {
				st.nameFails = 0
			} else {
				st.nameFails++
				if st.nameFails >= cfg.WatchFailLimit && time.Since(st.startedAt) > 2*time.Minute {
					log.Printf("watchdog: %q missing %dx -> reconnect", cfg.WatchName, st.nameFails)
					if _, err := deploy(st.servers, st.password, ""); err != nil {
						log.Println("watchdog: reconnect failed:", err)
					}
				}
			}
			st.mu.Unlock()
		}
	}
}

// heatSpikeFloor is the minimum leader heat (~two fresh kills) before a spike
// can override the dir_min_sec camera floor; spikeMinIntervalSec bounds how
// often a spike may cut, so a busy fight does not make the camera twitch.
const (
	heatSpikeFloor      = 1.8
	spikeMinIntervalSec = 8
)

// heatForCands restricts a heat map to the candidate slots (so the leader used
// for the spike check is someone actually followable right now, not a player
// who has since left).
func heatForCands(heat map[int]float64, cands []pipePlayer) map[int]float64 {
	out := map[int]float64{}
	for _, p := range cands {
		if v, ok := heat[p.Slot]; ok {
			out[p.Slot] = v
		}
	}
	return out
}

// pickByHeat chooses the hottest candidate. Heat is primary; a human beats a bot
// only as a tiebreaker (tiny nudge), and the current slot is nudged down so an
// exact tie does not needlessly re-pick the same player. Pure and unit-tested.
func pickByHeat(cands []pipePlayer, heat map[int]float64, humanBySlot map[int]bool, curSlot int) (pipePlayer, bool) {
	if len(cands) == 0 {
		return pipePlayer{}, false
	}
	best := -1
	var bestScore float64
	for i, p := range cands {
		s := heat[p.Slot]
		if humanBySlot[p.Slot] {
			s += 1e-6
		}
		if p.Slot == curSlot {
			s -= 1e-9
		}
		if best < 0 || s > bestScore {
			best, bestScore = i, s
		}
	}
	return cands[best], true
}

/* pickFromQuery selects a follow target from the client-provided player list:
 * only teams 1/2 (never spectators), never ourselves. The choice is weighted by
 * heat (recent-kill activity) so the camera sits on whoever is most likely to
 * make the next highlight; humans are preferred only as a tiebreaker. When
 * nobody is hot (e.g. the opening of a map) it falls back to the previous
 * human-preferred random pick, which spreads the camera around. */
func pickFromQuery(players []pipePlayer, self string, curSlot int, serverAddr string, heat map[int]float64) (int, string, string, int, bool) {
	selfC := strings.ToLower(cleanName(self))
	var cands []pipePlayer
	for _, p := range players {
		if p.Team != 1 && p.Team != 2 {
			continue
		}
		if strings.ToLower(cleanName(p.Name)) == selfC {
			continue
		}
		cands = append(cands, p)
	}
	if len(cands) == 0 {
		return 0, "", "", 0, false
	}
	pings := map[string]int{}
	if sp, alive := q3GetStatus(serverAddr); alive {
		for _, p := range sp {
			pings[strings.ToLower(cleanName(p.Name))] = p.Ping
		}
	}
	humanBySlot := map[int]bool{}
	for _, p := range cands {
		if pings[strings.ToLower(cleanName(p.Name))] > 0 {
			humanBySlot[p.Slot] = true
		}
	}

	var pick pipePlayer
	if _, top, _ := leaderByHeat(heatForCands(heat, cands)); top > 0 {
		// somebody is on the boil -> follow the heat.
		pick, _ = pickByHeat(cands, heat, humanBySlot, curSlot)
	} else {
		// quiet: prefer humans, pick at random, avoid repeating the current slot.
		pool := cands
		if cfg.PreferHumans && len(humanBySlot) > 0 {
			var humans []pipePlayer
			for _, p := range cands {
				if humanBySlot[p.Slot] {
					humans = append(humans, p)
				}
			}
			if len(humans) > 0 {
				pool = humans
			}
		}
		pick = pool[rng.Intn(len(pool))]
		if len(pool) > 1 {
			for guard := 0; guard < 8 && pick.Slot == curSlot; guard++ {
				pick = pool[rng.Intn(len(pool))]
			}
		}
	}
	name := cleanName(pick.Name)
	return pick.Slot, pick.Name, name, pings[strings.ToLower(name)], true
}
