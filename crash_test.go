package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetCrashState puts the reporter back to a clean slate between subtests. Not
// safe to run in parallel with real crashing goroutines; each test that touches
// it calls this in setup.
func resetCrashState(t *testing.T, dir string) {
	t.Helper()
	crashMu.Lock()
	lastCrash = nil
	crashTimes = crashTimes[:0]
	crashMu.Unlock()
	crashDir = dir
}

// TestGuardWritesReportAndTriggersRestart verifies the happy path: a wrapped
// goroutine that panics produces a crash-<ts>.log with the stack trace and the
// context snapshot, appends an entry to crashes.log, updates the /status
// summary, and triggers the restart callback.
func TestGuardWritesReportAndTriggersRestart(t *testing.T) {
	dir := t.TempDir()
	resetCrashState(t, dir)

	var restarted, exited int32
	crashRestartFn = func() error { atomic.AddInt32(&restarted, 1); return nil }
	crashExitFn = func(code int) { atomic.AddInt32(&exited, 1) }
	t.Cleanup(func() {
		crashRestartFn = reexecOnCrash
		crashExitFn = func(code int) { os.Exit(code) }
		crashDir = ""
	})

	done := make(chan struct{})
	goGuarded("test-subsystem", func() {
		defer close(done)
		panic("boom in test-subsystem: replay path exited on live scene")
	})
	<-done
	// handleCrash is called synchronously in the panicking goroutine, but the
	// restart callback runs after; wait briefly for the writes.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&restarted) > 0 && getLastCrash() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	lc := getLastCrash()
	if lc == nil {
		t.Fatal("expected lastCrash to be populated")
	}
	if lc.Subsystem != "test-subsystem" {
		t.Fatalf("subsystem = %q, want test-subsystem", lc.Subsystem)
	}
	if !strings.Contains(lc.Summary, "boom in test-subsystem") {
		t.Fatalf("summary %q must contain the panic value", lc.Summary)
	}
	if lc.ReportFile == "" {
		t.Fatal("expected a report file path")
	}
	data, err := os.ReadFile(lc.ReportFile)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	body := string(data)
	// stack trace present
	if !strings.Contains(body, "--- stack trace ---") {
		t.Error("report missing stack trace header")
	}
	if !strings.Contains(body, "goroutine ") {
		t.Error("report missing goroutine stack lines")
	}
	// runtime context snapshot present (the exact bug we needed after the
	// silent post-replay deaths).
	if !strings.Contains(body, "--- runtime context ---") {
		t.Error("report missing runtime context header")
	}
	for _, want := range []string{"live", "replay", "auto_director",
		"feed_recent", "goroutines", "memory"} {
		if !strings.Contains(body, want) {
			t.Errorf("report missing context field %q", want)
		}
	}
	// rolling log entry
	rolling, err := os.ReadFile(filepath.Join(dir, "crashes.log"))
	if err != nil {
		t.Fatalf("read crashes.log: %v", err)
	}
	if !strings.Contains(string(rolling), "test-subsystem") {
		t.Errorf("crashes.log missing subsystem entry: %s", rolling)
	}

	if atomic.LoadInt32(&restarted) != 1 {
		t.Errorf("restart callback called %d times, want 1", restarted)
	}
	if atomic.LoadInt32(&exited) != 1 {
		t.Errorf("exit callback called %d times, want 1", exited)
	}
}

// TestCrashLoopBreakerStops asserts that after crashLoopLimit+1 crashes inside
// the window the restart callback is NOT invoked -- a tight loop must not
// hammer ET/OBS forever. Restart_count still grows so operators can see it.
func TestCrashLoopBreakerStops(t *testing.T) {
	dir := t.TempDir()
	resetCrashState(t, dir)

	var restarts int32
	crashRestartFn = func() error { atomic.AddInt32(&restarts, 1); return nil }
	crashExitFn = func(code int) {}
	t.Cleanup(func() {
		crashRestartFn = reexecOnCrash
		crashExitFn = func(code int) { os.Exit(code) }
		crashDir = ""
	})

	// The breaker trips at the first crash where the rolling window count
	// EXCEEDS crashLoopLimit -- so the first crashLoopLimit crashes restart,
	// and every subsequent one is muzzled.
	total := crashLoopLimit + 3
	for i := 0; i < total; i++ {
		done := make(chan struct{})
		goGuarded("loop-test", func() {
			defer close(done)
			panic("repeat")
		})
		<-done
		// small yield so handleCrash finishes the counter update before the
		// next panic reads it (deterministic count under -race).
		time.Sleep(10 * time.Millisecond)
	}

	got := atomic.LoadInt32(&restarts)
	if int(got) != crashLoopLimit {
		t.Fatalf("restart callback called %d times, want %d (breaker should muzzle beyond %d in %v)",
			got, crashLoopLimit, crashLoopLimit, crashLoopWindow)
	}
	lc := getLastCrash()
	if lc == nil || lc.RestartCount < total {
		t.Fatalf("lastCrash.RestartCount = %d, want >= %d", lc.RestartCount, total)
	}
}

// TestCrashLoopResetsAfterWindow ensures the breaker forgives when the process
// has been stable long enough: entries older than crashLoopWindow are dropped
// and a fresh crash restarts again.
func TestCrashLoopResetsAfterWindow(t *testing.T) {
	dir := t.TempDir()
	resetCrashState(t, dir)

	// Fake a full window of past crashes, all older than crashLoopWindow.
	old := time.Now().Add(-2 * crashLoopWindow)
	crashMu.Lock()
	for i := 0; i < crashLoopLimit+2; i++ {
		crashTimes = append(crashTimes, old)
	}
	crashMu.Unlock()

	resetCrashLoopIfStable()

	crashMu.Lock()
	remaining := len(crashTimes)
	crashMu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected all old crashes to be dropped, %d remain", remaining)
	}
}

// TestCaptureContextSurvivesLockedMutex is the deadlock-safety guarantee: if a
// goroutine panics while holding a mutex (an inline critical section without a
// deferred Unlock), capturing the context must not block forever. We simulate
// that by taking st.mu on a helper goroutine and asserting captureCrashContext
// returns promptly with the "unavailable" annotation instead of hanging.
func TestCaptureContextSurvivesLockedMutex(t *testing.T) {
	release := make(chan struct{})
	acquired := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		st.mu.Lock()
		close(acquired)
		<-release
		st.mu.Unlock()
	}()
	<-acquired
	defer func() { close(release); wg.Wait() }()

	done := make(chan map[string]any, 1)
	go func() { done <- captureCrashContext("test") }()

	select {
	case snap := <-done:
		live, ok := snap["live"].(map[string]any)
		if !ok {
			t.Fatalf("live snapshot must be a map, got %T", snap["live"])
		}
		if note, _ := live["_note"].(string); !strings.Contains(note, "lock held") {
			t.Errorf("live[_note] = %q, want the lock-held annotation", note)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("captureCrashContext blocked on a held mutex (deadlock risk in the crash reporter)")
	}
}

// TestGuardRunsFnAndReturns ensures the wrapper is transparent on the happy
// path -- no report and no restart when nothing panics.
func TestGuardRunsFnAndReturns(t *testing.T) {
	resetCrashState(t, t.TempDir())
	crashRestartFn = func() error { t.Error("must not restart on clean return"); return nil }
	crashExitFn = func(int) { t.Error("must not exit on clean return") }
	t.Cleanup(func() {
		crashRestartFn = reexecOnCrash
		crashExitFn = func(code int) { os.Exit(code) }
		crashDir = ""
	})

	var ran int32
	guard("clean", func() { atomic.StoreInt32(&ran, 1) })
	if atomic.LoadInt32(&ran) != 1 {
		t.Fatal("guard must run fn to completion when it does not panic")
	}
	if getLastCrash() != nil {
		t.Fatal("no crash occurred, lastCrash should be nil")
	}
}
