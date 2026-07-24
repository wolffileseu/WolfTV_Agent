package main

import (
	"math"
	"testing"
)

func TestHeatScoresRecency(t *testing.T) {
	now := int64(100000)
	half := 8000.0 // 8s half-life in ms
	// slot 5: two kills, one now and one 8s ago -> 1.0 + 0.5 = 1.5
	// slot 9: one kill 16s ago -> 0.25
	samples := []killSample{
		{slot: 5, recv: now},
		{slot: 5, recv: now - 8000},
		{slot: 9, recv: now - 16000},
	}
	h := heatScores(samples, now, half)
	if math.Abs(h[5]-1.5) > 1e-9 {
		t.Errorf("slot5 heat = %v, want 1.5", h[5])
	}
	if math.Abs(h[9]-0.25) > 1e-9 {
		t.Errorf("slot9 heat = %v, want 0.25", h[9])
	}
	// a fresh double-kill must outrank an old single kill.
	if h[5] <= h[9] {
		t.Errorf("recent double should outrank old single: %v vs %v", h[5], h[9])
	}
}

func TestLeaderByHeat(t *testing.T) {
	slot, top, second := leaderByHeat(map[int]float64{3: 0.5, 7: 2.0, 9: 1.2})
	if slot != 7 || math.Abs(top-2.0) > 1e-9 || math.Abs(second-1.2) > 1e-9 {
		t.Errorf("leader=%d top=%v second=%v; want 7/2.0/1.2", slot, top, second)
	}
	if s, top, _ := leaderByHeat(map[int]float64{}); s != -1 || top != 0 {
		t.Errorf("empty heat should be (-1, 0), got (%d,%v)", s, top)
	}
}

func TestPickByHeatPrimary(t *testing.T) {
	cands := []pipePlayer{{Slot: 1, Team: 1}, {Slot: 2, Team: 2}, {Slot: 3, Team: 1}}
	heat := map[int]float64{1: 0.4, 2: 2.1, 3: 1.0}
	// heat dominates: slot 2 wins even though it is a bot and 1/3 are humans.
	humans := map[int]bool{1: true, 3: true}
	got, ok := pickByHeat(cands, heat, humans, -1)
	if !ok || got.Slot != 2 {
		t.Fatalf("heat-primary pick = slot %d, want 2", got.Slot)
	}
}

func TestPickByHeatHumanTiebreak(t *testing.T) {
	cands := []pipePlayer{{Slot: 1}, {Slot: 2}}
	heat := map[int]float64{1: 0, 2: 0} // nobody hot -> tie
	humans := map[int]bool{2: true}     // slot 2 is human
	got, _ := pickByHeat(cands, heat, humans, -1)
	if got.Slot != 2 {
		t.Errorf("on a heat tie the human should win, got slot %d", got.Slot)
	}
}

func TestHeatForCands(t *testing.T) {
	heat := map[int]float64{1: 5, 2: 3, 9: 9} // slot 9 left the game
	cands := []pipePlayer{{Slot: 1}, {Slot: 2}}
	got := heatForCands(heat, cands)
	if _, ok := got[9]; ok {
		t.Errorf("heat of a departed player must not survive: %v", got)
	}
	if got[1] != 5 || got[2] != 3 {
		t.Errorf("present candidates' heat lost: %v", got)
	}
}
