package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"time"
)

// Telemetry: last known client status via the pipeline.
type Telemetry struct {
	State     string `json:"state"`  // active|connecting|downloading|loading|disconnected
	Server    string `json:"server"` // ip:port
	Map       string `json:"map"`
	Following string `json:"following"`
	FPS       int    `json:"fps"`
	Percent   int    `json:"percent"` // download progress
	Reason    string `json:"reason"`  // disconnect reason

	// real follow state from the engine snapshot (nil = client too old)
	FollowSlot *int `json:"following_slot,omitempty"`
}

type pipePlayer struct {
	Slot int    `json:"slot"`
	Name string `json:"name"`
	Team int    `json:"team"` // 1=axis 2=allies 3=spectator
}

type pipeMsg struct {
	Ev      string       `json:"ev,omitempty"`
	Cmd     string       `json:"cmd,omitempty"`
	Line    string       `json:"line,omitempty"`
	Proto   int          `json:"proto,omitempty"`
	Id      int          `json:"id,omitempty"`
	Version string       `json:"version,omitempty"`
	What    string       `json:"what,omitempty"`
	Caps    []string     `json:"caps,omitempty"`
	Players []pipePlayer `json:"players,omitempty"`

	// action events
	Kind     string `json:"kind,omitempty"`
	SvTime   int    `json:"svtime,omitempty"`
	Attacker string `json:"attacker,omitempty"`
	Victim   string `json:"victim,omitempty"`
	Weapon   string `json:"weapon,omitempty"`
	Text     string `json:"text,omitempty"`
	File     string `json:"file,omitempty"`
	// absolute client numbers the client resolves for kills; nil for events
	// (selfkill omits attacker_slot) that carry no such slot.
	AttackerSlot *int `json:"attacker_slot,omitempty"`
	VictimSlot   *int `json:"victim_slot,omitempty"`

	Telemetry
}

// pipeLoop keeps a connection to this instance's control socket.
// Reconnects forever; marks pipeUp/lastEvent in the instance state. Only the
// live instance (feedEvents) forwards actions/demos to the event feed and only
// the directed instance resets the director on map change -- so the replay
// instance's re-played demos never pollute the live timeline or camera.
func (in *instance) pipeLoop() {
	if in.pipeAddr == "" {
		log.Printf("pipeline[%s]: disabled (pipe addr empty)", in.name)
		return
	}
	for {
		in.mu.Lock()
		// dial when we launched ET (etCmd) OR when we adopted an already-running
		// ET across a restart (adopted) -- otherwise the broadcast would be
		// stranded with no pipeline after an agent restart.
		running := in.etCmd != nil || in.adopted
		in.mu.Unlock()
		if !running {
			time.Sleep(2 * time.Second)
			continue
		}
		conn, err := net.DialTimeout("tcp", in.pipeAddr, 2*time.Second)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		log.Printf("pipeline[%s]: connected to client", in.name)
		in.mu.Lock()
		in.pipe = conn
		in.pipeUp = true
		in.lastEvent = time.Now()
		in.mu.Unlock()

		pipeSendRaw(conn, pipeMsg{Cmd: "hello", Proto: 1})

		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			var m pipeMsg
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				continue
			}
			in.mu.Lock()
			in.lastEvent = time.Now()
			switch m.Ev {
			case "hello":
				in.pipeCaps = map[string]bool{}
				for _, c := range m.Caps {
					in.pipeCaps[c] = true
				}
				in.clientVersion = m.Version
				log.Printf("pipeline[%s]: client hello, %s proto %d caps %v",
					in.name, m.Version, m.Proto, m.Caps)
			case "action":
				if in.feedEvents {
					// Capture the camera context at the instant the action lands:
					// who the (live, directed) instance is following right now. A
					// demo can only replay a highlight if the camera was on the
					// player who made it, so this is what decides replayable. in ==
					// st here (only the directed live instance feeds events) and
					// in.mu is held, so curTarget/curTargetSlot are consistent.
					aslot := -1
					if m.AttackerSlot != nil {
						aslot = *m.AttackerSlot
					}
					feed.addAction(ActionEvent{
						Kind: m.Kind, SvTime: m.SvTime, Attacker: m.Attacker,
						Victim: m.Victim, Weapon: m.Weapon, Text: m.Text,
						AttackerSlot: aslot,
						Followed:     in.curTarget, FollowedSlot: in.curTargetSlot,
						Server: in.currentServer, Map: in.tele.Map,
					})
				}
			case "evscan":
				// diagnostic: raw entity-event scan (cl_wtvEventScan 1).
				// parse separately, store in a ring for /events.
				if in.feedEvents {
					var e struct {
						EType  int `json:"etype"`
						Event  int `json:"event"`
						OEN    int `json:"oen"`
						OEN2   int `json:"oen2"`
						EParm  int `json:"eparm"`
						SvTime int `json:"svtime"`
					}
					if json.Unmarshal(sc.Bytes(), &e) == nil {
						feed.addScan(map[string]any{
							"etype": e.EType, "event": e.Event,
							"oen": e.OEN, "oen2": e.OEN2, "eparm": e.EParm,
							"svtime": e.SvTime,
						})
					}
				}
			case "demo":
				// parse demo events separately: they reuse the "state" json
				// key (start/stop) which collides with the telemetry state,
				// and carry seg_start_svtime/seg_end_svtime plus the "mod"
				// (fs_game) that recorded the demo -- only that mod can play
				// it back, so the replay instance is launched with it.
				if in.feedEvents {
					var d struct {
						State   string `json:"state"`
						File    string `json:"file"`
						Path    string `json:"path"`
						Mod     string `json:"mod"`
						Map     string `json:"map"`
						StartSv int    `json:"seg_start_svtime"`
						EndSv   int    `json:"seg_end_svtime"`
					}
					if json.Unmarshal(sc.Bytes(), &d) == nil {
						if d.State == "start" || d.State == "segment" {
							feed.addSegment(DemoSegment{File: d.File, Path: d.Path,
								Mod: d.Mod, Map: d.Map, StartSv: d.StartSv})
							log.Printf("demo: segment start %s (mod %q, svtime %d)", d.File, d.Mod, d.StartSv)
						} else if d.State == "stop" || d.State == "stopped" {
							feed.closeSegment(d.File, d.EndSv)
							log.Printf("demo: segment stop %s (svtime %d)", d.File, d.EndSv)
						}
					}
				}
			case "reply":
				if ch, ok := in.pending[m.Id]; ok {
					delete(in.pending, m.Id)
					select {
					case ch <- m.Players:
					default:
					}
				}
			case "status", "state", "mapchange", "download", "disconnect":
				wasDisconnected := in.tele.State == "disconnected"
				// "was connected" means we had a real, non-disconnected state
				// before. The empty initial state ("") is NOT connected: the
				// replay instance never joins a server, so its very first status
				// is state=disconnected -- that is normal idle, not a drop, and
				// must not be logged as a "client disconnect:" with empty reason.
				wasConnected := in.tele.State != "" && in.tele.State != "disconnected"
				// map change: server-side follow is lost -> reset director
				// so it re-specs and re-follows within seconds
				if m.Ev == "mapchange" ||
					(m.Map != "" && in.tele.Map != "" && m.Map != in.tele.Map) {
					if in.directs {
						resetDirectorLocked()
						log.Println("pipeline: map change ->", m.Map, "(director reset)")
					}
				}
				if m.State != "" {
					in.tele.State = m.State
				}
				if m.Server != "" {
					in.tele.Server = m.Server
					// an adopted instance has no currentServer yet -- learn it
					// from telemetry so the director's getstatus lookups work.
					if in.adopted && in.currentServer == "" {
						in.currentServer = m.Server
					}
				}
				if m.Map != "" {
					in.tele.Map = m.Map
				}
				if m.FollowSlot != nil {
					in.tele.FollowSlot = m.FollowSlot
				}
				if m.Following != "" {
					in.tele.Following = m.Following
				}
				if m.FPS > 0 {
					in.tele.FPS = m.FPS
				}
				in.tele.Percent = m.Percent
				if m.Ev == "disconnect" || m.State == "disconnected" {
					in.tele.State = "disconnected"
					in.tele.Reason = m.Reason
					if !wasDisconnected {
						in.discSince = time.Now() // start timer on TRANSITION only
						// A genuine drop is an explicit disconnect event or a
						// transition INTO disconnected from a connected state.
						// The idle replay instance reporting disconnected from
						// its initial "" is neither -- it never joins a server --
						// so that is not logged as a "client disconnect:" with an
						// empty reason. The watchdog timer (live only) is unchanged.
						if m.Ev == "disconnect" || wasConnected {
							log.Printf("pipeline[%s]: client disconnect: %s", in.name, m.Reason)
						}
					}
				} else if in.tele.State != "disconnected" {
					in.discSince = time.Time{} // recovered -> clear timer
				}
			}
			in.mu.Unlock()
		}
		conn.Close()
		in.mu.Lock()
		in.pipe = nil
		in.pipeUp = false
		in.mu.Unlock()
		log.Printf("pipeline[%s]: connection lost", in.name)
		time.Sleep(2 * time.Second)
	}
}

func pipeSendRaw(conn net.Conn, m pipeMsg) {
	b, _ := json.Marshal(m)
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, _ = conn.Write(append(b, '\n'))
}

// queryPlayers asks the client for the gamestate player list (slot/name/team).
// Returns ok=false when the client lacks the "query" capability or times out.
func queryPlayers(timeout time.Duration) ([]pipePlayer, bool) {
	st.mu.Lock()
	if !st.pipeUp || st.pipe == nil || !st.pipeCaps["query"] {
		st.mu.Unlock()
		return nil, false
	}
	st.reqID++
	id := st.reqID
	ch := make(chan []pipePlayer, 1)
	if st.pending == nil {
		st.pending = map[int]chan []pipePlayer{}
	}
	st.pending[id] = ch
	pipeSendRaw(st.pipe, pipeMsg{Cmd: "query", Id: id, What: "players"})
	st.mu.Unlock()

	select {
	case pl := <-ch:
		return pl, true
	case <-time.After(timeout):
		st.mu.Lock()
		delete(st.pending, id)
		st.mu.Unlock()
		return nil, false
	}
}

// pipeExecLocked sends a console command to this instance's client.
// Caller must hold in.mu.
func (in *instance) pipeExecLocked(line string) bool {
	if !in.pipeUp || in.pipe == nil {
		return false
	}
	pipeSendRaw(in.pipe, pipeMsg{Cmd: "exec", Line: line})
	return true
}
