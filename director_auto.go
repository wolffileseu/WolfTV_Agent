package main

// Automatic replay director (Part 3).
//
// A demo replays a highlight only if the live camera was on the player who made
// it (Part 1 marks those replayable). This picks the best such highlight, waits
// for a lull the way a sports broadcast does, and -- crucially -- prepares the
// clip ahead of time (load + seek + hold on the warm replay instance) so that
// when the lull comes only the OBS cut and playback remain.
//
// SAFETY: every decision here can only ever START a replay via the controller,
// which keeps OBS on the live scene until the very last step and always returns
// to live on any failure. Nothing here signals the live instance.

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	// autoMaxHighlightAgeSec: highlights older than this are never auto-replayed
	// (the moment has passed, and its demo segment may have rotated out).
	autoMaxHighlightAgeSec = 180
	// maxPreparedHoldSec: if a prepared clip waits this long for a lull that
	// never comes, give up on it (mark it used) and release the instance.
	maxPreparedHoldSec = 120
)

// autoCandidate is one replayable highlight joined to the demo segment that
// holds it, with everything needed to schedule and play it.
type autoCandidate struct {
	File       string `json:"file"`
	Path       string `json:"path"`
	Mod        string `json:"mod"`
	Map        string `json:"map"`
	PlayerSlot int    `json:"player_slot"`
	Player     string `json:"player"`
	Score      int    `json:"score"`
	SvTime     int    `json:"svtime"`
	OffsetMs   int    `json:"offset_ms"`
	AgeMs      int64  `json:"age_ms"`
}

// candKey uniquely identifies a highlight so the same one is never replayed
// twice.
func candKey(c autoCandidate) string {
	return fmt.Sprintf("%s|%d|%d", c.File, c.SvTime, c.PlayerSlot)
}

// buildAutoCandidates joins replayable highlights to their segments, restricted
// to the current live map, keeping only ones with a resolvable, safe demo path.
// Pure so it can be tested against synthetic feeds.
func buildAutoCandidates(segs []DemoSegment, hls []Highlight, now int64, liveMap string) []autoCandidate {
	var out []autoCandidate
	for _, seg := range segs {
		if liveMap != "" && seg.Map != liveMap {
			continue
		}
		mod, _, err := resolveSegmentMod(seg)
		if err != nil {
			continue
		}
		relPath := demoPlayArg(seg)
		if !validDemoPath(relPath) {
			continue
		}
		for _, h := range hls {
			if !h.Replayable || !svtimeInSegment(h.SvTime, seg) {
				continue
			}
			// an open segment (EndSv 0) has no upper svtime bound, so also
			// require the highlight's map to match -- otherwise a highlight from
			// the next map could attach to the still-open current segment.
			if h.Map != "" && seg.Map != "" && h.Map != seg.Map {
				continue
			}
			out = append(out, autoCandidate{
				File: seg.File, Path: relPath, Mod: mod, Map: seg.Map,
				PlayerSlot: h.FollowedSlot, Player: h.Player,
				Score: h.Score, SvTime: h.SvTime,
				OffsetMs: offsetFromSvtime(h.SvTime, seg.StartSv),
				AgeMs:    now - h.Recv,
			})
		}
	}
	return out
}

// candidateEligible reports whether a candidate may be auto-replayed right now:
// old enough that its demo data is on disk, not older than the horizon, not
// already used, and not the same player as the previous replay.
func candidateEligible(c autoCandidate, dc DirectorConfig, lastPlayerSlot int, used map[string]bool) bool {
	minAge := int64(dc.MinHighlightAgeSec) * 1000
	maxAge := int64(autoMaxHighlightAgeSec) * 1000
	if c.AgeMs < minAge || c.AgeMs > maxAge {
		return false
	}
	if used[candKey(c)] {
		return false
	}
	if lastPlayerSlot >= 0 && c.PlayerSlot == lastPlayerSlot {
		return false
	}
	return true
}

// betterCandidate ranks candidates: higher score first (a triple beats a
// double), then more recent.
func betterCandidate(a, b autoCandidate) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.AgeMs < b.AgeMs
}

// selectAutoCandidate picks the best eligible candidate. Pure and unit-tested.
func selectAutoCandidate(cands []autoCandidate, dc DirectorConfig, lastPlayerSlot int, used map[string]bool, now int64) (autoCandidate, bool) {
	var best autoCandidate
	found := false
	for _, c := range cands {
		if !candidateEligible(c, dc, lastPlayerSlot, used) {
			continue
		}
		if !found || betterCandidate(c, best) {
			best, found = c, true
		}
	}
	return best, found
}

// candidateToJob turns the chosen candidate into a replay job using the runtime
// window from the director config.
func candidateToJob(c autoCandidate, dc DirectorConfig, absPath string) replayJob {
	return replayJob{
		file: c.File, path: c.Path, mod: c.Mod, absPath: absPath,
		offsetMs: c.OffsetMs,
		preMs:    dc.PreSec * 1000, postMs: dc.PostSec * 1000, speed: dc.Speed,
	}
}

/* --------------------------------- state ----------------------------------- */

type autoDirector struct {
	mu                 sync.Mutex
	curMap             string
	mapReplays         int
	lastPlayerSlot     int
	usedHighlights     map[string]bool
	preparedKey        string
	preparedPlayerSlot int
	preparedMap        string
	lastCandidates     []autoCandidate
}

var auto = &autoDirector{lastPlayerSlot: -1, preparedPlayerSlot: -1, usedHighlights: map[string]bool{}}

func cloneSet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// autoDirectorLoop drives the automatic replay director on a 1s tick. It does
// nothing unless auto_replay is enabled in director.json.
func autoDirectorLoop() {
	for {
		time.Sleep(1 * time.Second)
		dc := dcfg.get()
		// inert unless auto-replay is on AND a replay instance exists to drive.
		if !dc.AutoReplay || !cfg.ReplayEnabled || rp == nil {
			continue
		}
		auto.tick(dc)
	}
}

func (a *autoDirector) tick(dc DirectorConfig) {
	now := time.Now().UnixMilli()

	st.mu.Lock()
	liveMap := st.tele.Map
	liveActive := st.tele.State == "active"
	livePipe := st.pipeUp
	st.mu.Unlock()

	// A map change invalidates per-map counters, used-highlight memory, and any
	// preparation for the old map.
	a.mu.Lock()
	if liveMap != a.curMap {
		a.curMap = liveMap
		a.mapReplays = 0
		a.usedHighlights = map[string]bool{}
		a.lastPlayerSlot = -1
		a.preparedKey = ""
	}
	a.mu.Unlock()

	// The broadcast must be healthy to do anything: live pipeline up and in a
	// map (not mid map change / disconnect). Drop any hold otherwise.
	if !livePipe || !liveActive {
		if replay.isPrepared() {
			replay.discardPrepared()
		}
		return
	}

	quietMs := feed.quietForMs(now)

	cands := buildAutoCandidates(feed.segmentsSnapshot(), feed.highlightsSnapshot(), now, liveMap)
	a.mu.Lock()
	a.lastCandidates = cands
	lastSlot := a.lastPlayerSlot
	used := cloneSet(a.usedHighlights)
	mapReplays := a.mapReplays
	a.mu.Unlock()
	best, ok := selectAutoCandidate(cands, dc, lastSlot, used, now)
	capReached := dc.PerMapCap > 0 && mapReplays >= dc.PerMapCap

	// ---- something is prepared and holding ----
	if replay.isPrepared() {
		if time.Since(replay.preparedSince()) > maxPreparedHoldSec*time.Second {
			log.Println("auto: prepared hold timed out (no lull) -> discarding")
			a.markPreparedUsed()
			replay.discardPrepared()
			return
		}
		if quietMs >= int64(dc.LullSec)*1000 {
			if replay.triggerPrepared() {
				a.onTriggered()
				log.Printf("auto: lull %ds -> cutting to replay", quietMs/1000)
			}
		}
		return
	}

	// ---- nothing prepared: consider preparing ----
	if replay.isActive() {
		return // a manual replay is running -- stay out of the way
	}
	if capReached || !ok {
		return
	}
	if time.Since(replay.lastDoneTime()) < time.Duration(dc.MinIntervalSec)*time.Second {
		return // honour the minimum spacing between replays
	}

	absPath, err := demoAbsPath(cfg.LiveHomepath, cfg.ReplayDemoDir, best.Path)
	if err != nil {
		return
	}
	if !cfg.DryRun {
		if _, serr := os.Stat(absPath); serr != nil {
			return // demo not on disk yet / rotated out -- skip quietly
		}
	}

	if replay.prepareAuto(candidateToJob(best, dc, absPath)) {
		a.mu.Lock()
		a.preparedKey = candKey(best)
		a.preparedPlayerSlot = best.PlayerSlot
		a.preparedMap = best.Map
		a.mu.Unlock()
		log.Printf("auto: preparing replay %s (%s score %d age %ds)",
			best.File, best.Player, best.Score, best.AgeMs/1000)
	}
}

// onTriggered records that the prepared clip was played: count it against the
// per-map cap, remember its player (never twice in a row) and mark it used.
func (a *autoDirector) onTriggered() {
	a.mu.Lock()
	a.mapReplays++
	if a.preparedKey != "" {
		a.usedHighlights[a.preparedKey] = true
	}
	a.lastPlayerSlot = a.preparedPlayerSlot
	a.preparedKey = ""
	a.mu.Unlock()
}

// markPreparedUsed marks a prepared-but-abandoned clip used so the director does
// not immediately re-prepare the same one.
func (a *autoDirector) markPreparedUsed() {
	a.mu.Lock()
	if a.preparedKey != "" {
		a.usedHighlights[a.preparedKey] = true
		a.preparedKey = ""
	}
	a.mu.Unlock()
}

/* --------------------------------- status ---------------------------------- */

// candidateStatus is a candidate annotated with whether it is currently
// eligible, for the panel.
type candidateStatus struct {
	autoCandidate
	AgeSec   int64 `json:"age_sec"`
	Eligible bool  `json:"eligible"`
}

// GET /director/status -> what the director is currently thinking.
func handleDirectorStatus(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	dc := dcfg.get()
	now := time.Now().UnixMilli()
	quietMs := feed.quietForMs(now)

	auto.mu.Lock()
	cands := make([]autoCandidate, len(auto.lastCandidates))
	copy(cands, auto.lastCandidates)
	lastSlot := auto.lastPlayerSlot
	used := cloneSet(auto.usedHighlights)
	mapReplays := auto.mapReplays
	auto.mu.Unlock()

	// rank the same way selection does, and annotate eligibility
	views := make([]candidateStatus, 0, len(cands))
	for _, c := range cands {
		views = append(views, candidateStatus{
			autoCandidate: c,
			AgeSec:        c.AgeMs / 1000,
			Eligible:      candidateEligible(c, dc, lastSlot, used),
		})
	}
	sortCandidateStatus(views)

	lastDone := replay.lastDoneTime()
	var lastReplayAt, nextEligibleAt int64
	if !lastDone.IsZero() {
		lastReplayAt = lastDone.Unix()
		nextEligibleAt = lastDone.Add(time.Duration(dc.MinIntervalSec) * time.Second).Unix()
	}

	writeJSON(w, 200, map[string]any{
		"auto_enabled":     dc.AutoReplay,
		"candidates":       views,
		"last_replay_at":   lastReplayAt,
		"next_eligible_at": nextEligibleAt,
		"quiet_for_sec":    quietMs / 1000,
		"lull_sec":         dc.LullSec,
		"map_replays":      mapReplays,
		"per_map_cap":      dc.PerMapCap,
		"prepared":         replay.preparedView(),
	})
}

// sortCandidateStatus orders candidates best-first (score desc, then recency),
// an insertion sort -- the list is short (highlights within the current map).
func sortCandidateStatus(v []candidateStatus) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && betterCandidate(v[j].autoCandidate, v[j-1].autoCandidate); j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
