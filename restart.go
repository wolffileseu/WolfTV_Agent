package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Agent self-restart + adoption of an already-running ET, so a config change
// that needs a restart does not require remote access to the box.
//
// ET HANDLING (the deliberate choice): /restart LEAVES the live ET running and
// re-execs the agent; the new process re-attaches to ET through the control
// pipe (adoptLiveET). Killing ET would drop the broadcast for the whole restart;
// leaving it up is seamless AS LONG AS the pipeline can reconnect -- and the
// stock pipeLoop could NOT (it dialed only when it owned the child handle), so
// adoption was added to make re-attach actually work. This path needs a live
// check on the Windows box; it cannot be exercised without a real ET + OBS.

// reexec spawns a fresh copy of this agent with the same executable, arguments
// and working directory, inheriting the console streams. The child outlives the
// parent (no job object on Windows; orphaned-but-alive on exit).
func reexec() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Dir = cwd
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

// POST /restart -> re-exec the agent process. Refuses while a replay is in
// flight so a restart can't strand the broadcast on the replay scene.
func handleRestart(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	if rp != nil && replay.isActive() {
		writeJSON(w, 409, map[string]string{"error": "a replay is in flight; retry once it finishes"})
		return
	}
	// Spawn the replacement first, so we only exit if it actually started. The
	// child retries the HTTP bind until this process releases the port.
	if err := reexec(); err != nil {
		log.Println("restart: re-exec failed, staying up:", err)
		writeJSON(w, 500, map[string]string{"error": "re-exec failed: " + err.Error()})
		return
	}
	log.Println("restart: replacement spawned; live ET left running for it to adopt -- exiting")
	writeJSON(w, 200, map[string]any{"ok": true})
	// exit after the response has a moment to flush.
	go func() {
		time.Sleep(400 * time.Millisecond)
		os.Exit(0)
	}()
}

// pipeReachable reports whether something accepts a connection on addr, i.e.
// whether an ET control pipe is up. Used to detect a running ET at startup and,
// in the watchdog, to confirm a suspected-dead adopted ET is really gone before
// relaunching (double-spawn guard).
func pipeReachable(addr string) bool {
	if addr == "" {
		return false
	}
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// adoptLiveET checks whether the live ET is already running (its control pipe
// accepts a connection) and, if so, marks the live instance adopted so pipeLoop
// re-attaches and the broadcast survives an agent restart. It recovers the
// server pool / password / overlay from live-state.json so the watchdog can
// RELAUNCH the adopted ET if it later dies -- adoption changes what the watchdog
// knows, not whether it acts. A brief restart-grace lets the pipe reattach
// before the watchdog judges liveness. On a cold start nothing answers the pipe
// and this is a no-op. Called before pipeLoop.
func adoptLiveET() {
	if !pipeReachable(cfg.PipeAddr) {
		return // no ET running -> normal cold start, wait for /start
	}
	st.mu.Lock()
	st.desired = true
	st.adopted = true
	st.restartDeadline = time.Now().Add(adoptGraceSec * time.Second)
	if ls, err := loadLiveState(); err == nil {
		st.servers = ls.Servers
		st.password = ls.Password
		st.overlayURL = ls.OverlayURL
		if ls.CurrentServer != "" {
			st.currentServer = ls.CurrentServer
		}
		log.Printf("adopt: recovered live-state (%d servers, on %q) -- watchdog can relaunch if ET dies",
			len(ls.Servers), ls.CurrentServer)
	} else {
		log.Printf("adopt: WARNING no recoverable live-state (%v) -- if this ET dies the watchdog "+
			"will alert but cannot relaunch until a panel /start", err)
	}
	st.mu.Unlock()
	log.Printf("adopt: live ET already running on %s -- re-attaching, broadcast preserved", cfg.PipeAddr)
}

// serveWithRetry binds the listen address, retrying briefly if it is still held
// by a just-exiting previous process (the /restart handoff), then serves.
func serveWithRetry(addr string, h http.Handler) error {
	var (
		ln  net.Listener
		err error
	)
	for i := 0; i < 30; i++ {
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		if !strings.Contains(err.Error(), "in use") &&
			!strings.Contains(err.Error(), "already") {
			return err // a real bind error, not the handoff race
		}
		if i == 0 {
			log.Printf("listen: %s busy (previous process exiting?), retrying", addr)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if ln == nil {
		return err
	}
	return http.Serve(ln, h)
}
