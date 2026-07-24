package main

import "testing"

func newTestFeed() *eventFeed {
	return &eventFeed{
		kills:     map[string]*killTrack{},
		maxEvents: 500, maxScans: 300, maxSeg: 50, maxHl: 100,
	}
}

func TestHighlightReplayable(t *testing.T) {
	cases := []struct {
		name     string
		subjSlot int
		subjName string
		follSlot int
		follName string
		want     bool
	}{
		{"slot match", 5, "Rambo", 5, "Rambo", true},
		{"slot mismatch beats name", 5, "Rambo", 6, "Rambo", false},
		{"slot match wins even if names differ", 5, "A", 5, "B", true},
		{"not following anyone", 5, "Rambo", -1, "", false},
		{"name fallback match (no subject slot)", -1, "^1Rambo", -1, "rambo", true},
		{"name fallback mismatch", -1, "Rambo", -1, "Doc", false},
		{"no slot no name", -1, "", -1, "", false},
		{"subject has no slot, follower does -> name fallback", -1, "Rambo", 3, "Rambo", true},
	}
	for _, c := range cases {
		if got := highlightReplayable(c.subjSlot, c.subjName, c.follSlot, c.follName); got != c.want {
			t.Errorf("%s: highlightReplayable(%d,%q,%d,%q)=%v want %v",
				c.name, c.subjSlot, c.subjName, c.follSlot, c.follName, got, c.want)
		}
	}
}

// a double kill by the followed player is replayable; by someone else is not.
func TestAddActionReplayableMultikill(t *testing.T) {
	f := newTestFeed()
	// camera on slot 5; two quick kills by slot 5 -> replayable double kill
	f.addAction(ActionEvent{Kind: "kill", Attacker: "Rambo", AttackerSlot: 5,
		SvTime: 1000, Followed: "Rambo", FollowedSlot: 5})
	f.addAction(ActionEvent{Kind: "kill", Attacker: "Rambo", AttackerSlot: 5,
		SvTime: 2000, Followed: "Rambo", FollowedSlot: 5})
	hls := f.highlightsSnapshot()
	if len(hls) != 1 {
		t.Fatalf("want 1 highlight, got %d", len(hls))
	}
	h := hls[0]
	if h.Kind != "multikill" || h.Count != 2 {
		t.Fatalf("want multikill count 2, got %s count %d", h.Kind, h.Count)
	}
	if !h.Replayable {
		t.Errorf("double kill by the followed player must be replayable")
	}
	if h.PlayerSlot != 5 || h.FollowedSlot != 5 {
		t.Errorf("player_slot/followed_slot: got %d/%d want 5/5", h.PlayerSlot, h.FollowedSlot)
	}
}

func TestAddActionNotReplayableWrongCamera(t *testing.T) {
	f := newTestFeed()
	// camera on slot 9; the double kill is by slot 5 -> not replayable
	f.addAction(ActionEvent{Kind: "kill", Attacker: "Rambo", AttackerSlot: 5,
		SvTime: 1000, Followed: "Doc", FollowedSlot: 9})
	f.addAction(ActionEvent{Kind: "kill", Attacker: "Rambo", AttackerSlot: 5,
		SvTime: 2000, Followed: "Doc", FollowedSlot: 9})
	hls := f.highlightsSnapshot()
	if len(hls) != 1 {
		t.Fatalf("want 1 highlight, got %d", len(hls))
	}
	if hls[0].Replayable {
		t.Errorf("double kill made off-camera must not be replayable")
	}
}
