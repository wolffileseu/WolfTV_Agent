package main

import (
	"testing"
	"time"
)

// The decision that failed overnight, plus the whole matrix. The key rows are
// "adopted + ET dead + server known -> relaunch" (was: silence) and
// "adopted + ET dead + server unknown -> error" (was: silence).
func TestWatchdogAction(t *testing.T) {
	cases := []struct {
		name        string
		etAlive     bool
		serverKnown bool
		grace       bool
		want        wdAction
	}{
		{"alive -> leave (no double-spawn)", true, true, false, wdLeave},
		{"alive during grace -> leave", true, true, true, wdLeave},
		{"alive, no server -> leave", true, false, false, wdLeave},
		{"dead within restart grace -> leave", false, true, true, wdLeave},
		{"dead within grace, no server -> leave", false, false, true, wdLeave},

		// THE FIX: adopted+dead with a known server must relaunch, not go silent.
		{"dead, server known, past grace -> relaunch", false, true, false, wdRelaunch},

		// THE OTHER FIX: dead with nowhere to relaunch is a loud error, not silence.
		{"dead, server unknown, past grace -> error", false, false, false, wdErrorLog},
	}
	for _, c := range cases {
		if got := watchdogAction(c.etAlive, c.serverKnown, c.grace); got != c.want {
			t.Errorf("%s: watchdogAction(%v,%v,%v)=%s want %s",
				c.name, c.etAlive, c.serverKnown, c.grace, got, c.want)
		}
	}
}

// watchdogAction must NEVER return wdLeave for a dead, past-grace instance --
// that was the silent-death bug (a dead instance left alone forever).
func TestWatchdogNeverSilentOnDeadPastGrace(t *testing.T) {
	for _, serverKnown := range []bool{true, false} {
		if got := watchdogAction(false, serverKnown, false); got == wdLeave {
			t.Errorf("dead + past grace (serverKnown=%v) must act, got wdLeave (silence)", serverKnown)
		}
	}
}

func TestLiveEtAlive(t *testing.T) {
	dead := 30 * time.Second
	cases := []struct {
		name      string
		etCmd     bool
		adopted   bool
		pipeUp    bool
		sinceLast time.Duration
		want      bool
	}{
		{"spawned with handle -> alive", true, false, false, time.Hour, true},
		{"spawned handle gone -> dead", false, false, false, 0, false},
		{"adopted, pipe up -> alive", false, true, true, time.Hour, true},
		{"adopted, pipe just dropped -> alive (transient)", false, true, false, 5 * time.Second, true},
		{"adopted, pipe down long -> dead", false, true, false, 5 * time.Minute, false},
		{"adopted, never connected -> dead", false, true, false, 1 << 40, false},
	}
	for _, c := range cases {
		if got := liveEtAlive(c.etCmd, c.adopted, c.pipeUp, c.sinceLast, dead); got != c.want {
			t.Errorf("%s: liveEtAlive=%v want %v", c.name, got, c.want)
		}
	}
}

// The exact overnight scenario, end to end through both helpers: an adopted ET
// whose pipe has been down for minutes, with a recovered server pool, must be
// judged dead and relaunched.
func TestOvernightScenarioRelaunches(t *testing.T) {
	dead := 30 * time.Second
	etAlive := liveEtAlive(false /*no handle*/, true /*adopted*/, false /*pipe down*/, 4*time.Hour, dead)
	if etAlive {
		t.Fatal("an adopted ET with the pipe down for 4h must be judged dead")
	}
	got := watchdogAction(etAlive, true /*server known from live-state*/, false /*grace expired*/)
	if got != wdRelaunch {
		t.Fatalf("overnight case must relaunch, got %s", got)
	}
}
