package main

import (
	"log"
	"strings"
	"sync"
	"time"
)

/* Event feed + highlight detection.
 * Collects "action" and "demo" pipeline events into a rolling history,
 * derives highlights (multikills, streaks), and exposes everything via
 * /events for the dev panel. No replay playback yet -- this is the
 * timeline the replay director will later consume. */

type ActionEvent struct {
	Seq          int64  `json:"seq"`
	Recv         int64  `json:"recv"` // agent unix ms when received
	Kind         string `json:"kind"`
	SvTime       int    `json:"svtime"` // server time ms
	Attacker     string `json:"attacker,omitempty"`
	Victim       string `json:"victim,omitempty"`
	Weapon       string `json:"weapon,omitempty"`
	Text         string `json:"text,omitempty"`
	AttackerSlot int    `json:"attacker_slot,omitempty"` // absolute client num, -1 if none
	Server       string `json:"server,omitempty"`
	Map          string `json:"map,omitempty"`
	// Camera context captured when the event landed: who the live instance was
	// following. FollowedSlot is -1 for free cam / not following.
	Followed     string `json:"followed,omitempty"`
	FollowedSlot int    `json:"followed_slot"`
	Highlight    string `json:"highlight,omitempty"` // set if this triggered one
	Score        int    `json:"score,omitempty"`     // highlight weight
}

type DemoSegment struct {
	File    string `json:"file"`
	Path    string `json:"path,omitempty"` // homepath-relative path as reported by the client (informational)
	Mod     string `json:"mod,omitempty"`  // fs_game that RECORDED this demo -- only that mod can play it
	Map     string `json:"map"`
	StartSv int    `json:"seg_start_svtime"`
	EndSv   int    `json:"seg_end_svtime,omitempty"`
	Recv    int64  `json:"recv"`
}

type Highlight struct {
	Kind   string `json:"kind"` // multikill, streak, dynamite, objective...
	Player string `json:"player"`
	SvTime int    `json:"svtime"`
	Score  int    `json:"score"`
	Count  int    `json:"count,omitempty"`
	Text   string `json:"text,omitempty"`
	Server string `json:"server,omitempty"`
	Map    string `json:"map,omitempty"`
	Recv   int64  `json:"recv"`

	// Camera context at the moment of the highlight, so the panel can show
	// which highlights are actually replayable and the auto-director can filter
	// to only those. A demo replays a highlight only if the camera was on the
	// player who made it.
	Followed     string `json:"followed,omitempty"` // followed player's name
	FollowedSlot int    `json:"followed_slot"`      // -1 if not following anyone
	PlayerSlot   int    `json:"player_slot"`        // subject's slot, -1 if unknown
	Replayable   bool   `json:"replayable"`         // camera was on the subject
}

type killTrack struct {
	times []int // svtimes of recent kills by this attacker
}

type eventFeed struct {
	mu         sync.Mutex
	seq        int64
	events     []ActionEvent // ring
	segments   []DemoSegment // ring
	highlights []Highlight   // ring
	kills      map[string]*killTrack
	scans      []map[string]any
	maxScans   int
	maxEvents  int
	maxSeg     int
	maxHl      int
}

var feed = &eventFeed{
	kills:     map[string]*killTrack{},
	maxEvents: 500,
	maxScans:  300,
	maxSeg:    50,
	maxHl:     100,
}

// tuning (could move to config later)
const (
	multiKillWindowMs = 5000 // kills within this window count as a multi
	multiKillMin      = 2    // >=2 kills = multikill highlight
	streakMin         = 5    // kills without dying (approx) for a streak
)

// highlightReplayable reports whether a highlight can be replayed from the demo:
// true only if the camera was following the highlight's subject. Slot matching
// is preferred (robust against duplicate names); a name match is the fallback
// for subjects with no known slot (objective events). A subject with neither a
// slot nor a name (e.g. an unattributed dynamite) is never replayable.
func highlightReplayable(subjectSlot int, subjectName string, followedSlot int, followedName string) bool {
	if subjectSlot >= 0 && followedSlot >= 0 {
		return subjectSlot == followedSlot
	}
	if subjectName != "" && followedName != "" {
		return strings.EqualFold(cleanName(subjectName), cleanName(followedName))
	}
	return false
}

func (f *eventFeed) addAction(a ActionEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	a.Seq = f.seq
	a.Recv = time.Now().UnixMilli()

	// multikill detection on kills with a known human-ish attacker
	if a.Kind == "kill" && a.Attacker != "" {
		key := strings.ToLower(cleanName(a.Attacker))
		kt := f.kills[key]
		if kt == nil {
			kt = &killTrack{}
			f.kills[key] = kt
		}
		// drop old
		var recent []int
		for _, t := range kt.times {
			if a.SvTime-t <= multiKillWindowMs {
				recent = append(recent, t)
			}
		}
		recent = append(recent, a.SvTime)
		kt.times = recent
		if len(recent) >= multiKillMin {
			score := len(recent) * 3 // double=6, triple=9...
			a.Highlight = "multikill"
			a.Score = score
			// subject of a multikill is the attacker; match by slot.
			f.pushHighlightLocked(Highlight{
				Kind: "multikill", Player: cleanName(a.Attacker),
				SvTime: a.SvTime, Score: score, Count: len(recent),
				Server: a.Server, Map: a.Map, Recv: a.Recv,
				Followed: a.Followed, FollowedSlot: a.FollowedSlot,
				PlayerSlot: a.AttackerSlot,
				Replayable: highlightReplayable(a.AttackerSlot, a.Attacker, a.FollowedSlot, a.Followed),
			})
		}
	}

	// objective-class highlights
	switch a.Kind {
	case "dynamite_explode":
		// no attributed player -> never camera-replayable.
		a.Highlight, a.Score = "dynamite", 8
		f.pushHighlightLocked(Highlight{Kind: "dynamite", SvTime: a.SvTime,
			Score: 8, Server: a.Server, Map: a.Map, Recv: a.Recv,
			Followed: a.Followed, FollowedSlot: a.FollowedSlot, PlayerSlot: -1,
			Replayable: false})
	case "objective_taken", "objective_secured", "checkpoint":
		// objective events carry a player name but no slot; match by name.
		a.Highlight, a.Score = "objective", 5
		f.pushHighlightLocked(Highlight{Kind: "objective", Player: cleanName(a.Attacker),
			SvTime: a.SvTime, Score: 5, Text: a.Text, Server: a.Server, Map: a.Map, Recv: a.Recv,
			Followed: a.Followed, FollowedSlot: a.FollowedSlot, PlayerSlot: -1,
			Replayable: highlightReplayable(-1, a.Attacker, a.FollowedSlot, a.Followed)})
	}

	f.events = append(f.events, a)
	if len(f.events) > f.maxEvents {
		f.events = f.events[len(f.events)-f.maxEvents:]
	}
}

func (f *eventFeed) pushHighlightLocked(h Highlight) {
	f.highlights = append(f.highlights, h)
	if len(f.highlights) > f.maxHl {
		f.highlights = f.highlights[len(f.highlights)-f.maxHl:]
	}
	log.Printf("highlight: %s %s (score %d) svtime %d",
		h.Kind, h.Player, h.Score, h.SvTime)
}

func (f *eventFeed) addSegment(s DemoSegment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s.Recv = time.Now().UnixMilli()
	// close previous open segment of same file? just append; agent maps by svtime
	f.segments = append(f.segments, s)
	if len(f.segments) > f.maxSeg {
		f.segments = f.segments[len(f.segments)-f.maxSeg:]
	}
}

func (f *eventFeed) closeSegment(file string, endSv int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.segments) - 1; i >= 0; i-- {
		if f.segments[i].File == file && f.segments[i].EndSv == 0 {
			f.segments[i].EndSv = endSv
			return
		}
	}
}

func (f *eventFeed) addScan(s map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans = append(f.scans, s)
	if len(f.scans) > f.maxScans {
		f.scans = f.scans[len(f.scans)-f.maxScans:]
	}
}

// segmentsSnapshot returns a copy of the current demo segments (oldest first).
func (f *eventFeed) segmentsSnapshot() []DemoSegment {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]DemoSegment, len(f.segments))
	copy(out, f.segments)
	return out
}

// highlightsSnapshot returns a copy of the current highlights (oldest first).
func (f *eventFeed) highlightsSnapshot() []Highlight {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Highlight, len(f.highlights))
	copy(out, f.highlights)
	return out
}

// findSegment returns the most recent segment matching a demo basename.
func (f *eventFeed) findSegment(file string) (DemoSegment, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.segments) - 1; i >= 0; i-- {
		if f.segments[i].File == file {
			return f.segments[i], true
		}
	}
	return DemoSegment{}, false
}

// snapshot for /events
func (f *eventFeed) snapshot(sinceSeq int64) ([]ActionEvent, []Highlight, []DemoSegment, []map[string]any, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var evs []ActionEvent
	for _, e := range f.events {
		if e.Seq > sinceSeq {
			evs = append(evs, e)
		}
	}
	hl := make([]Highlight, len(f.highlights))
	copy(hl, f.highlights)
	seg := make([]DemoSegment, len(f.segments))
	copy(seg, f.segments)
	scans := make([]map[string]any, len(f.scans))
	copy(scans, f.scans)
	return evs, hl, seg, scans, f.seq
}
