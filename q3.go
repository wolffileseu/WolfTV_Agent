package main

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"regexp"
	"strings"
	"time"
)

var colorRE = regexp.MustCompile(`\^.`)

type q3Player struct {
	Name string // raw incl. color codes
	Ping int
}

func cleanName(s string) string {
	return strings.TrimSpace(colorRE.ReplaceAllString(s, ""))
}

// parseInfoString parses a Quake3 "\key\value\key\value" infostring into a map.
func parseInfoString(s string) map[string]string {
	m := map[string]string{}
	parts := strings.Split(strings.Trim(s, `\`), `\`)
	for i := 0; i+1 < len(parts); i += 2 {
		m[parts[i]] = parts[i+1]
	}
	return m
}

// q3GetStatusInfo queries a server and returns both the parsed serverinfo
// (sv_hostname, gamename, mapname, ...) and the player list.
func q3GetStatusInfo(addr string) (map[string]string, []q3Player, bool) {
	conn, err := net.DialTimeout("udp", addr, 2*time.Second)
	if err != nil {
		return nil, nil, false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write([]byte("\xff\xff\xff\xffgetstatus\n"))
	buf := make([]byte, 16384)
	n, err := conn.Read(buf)
	if err != nil || n < 4 || !bytes.HasPrefix(buf, []byte("\xff\xff\xff\xff")) {
		return nil, nil, false
	}
	lines := strings.Split(string(buf[:n]), "\n")
	var info map[string]string
	if len(lines) > 1 {
		info = parseInfoString(lines[1]) // line 0 = header, line 1 = serverinfo
	}
	var players []q3Player
	for i, l := range lines {
		if i < 2 { // header + serverinfo
			continue
		}
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		var score, ping int
		if _, err := fmt.Sscanf(l, "%d %d", &score, &ping); err != nil {
			continue
		}
		q1 := strings.Index(l, `"`)
		q2 := strings.LastIndex(l, `"`)
		if q1 < 0 || q2 <= q1 {
			continue
		}
		players = append(players, q3Player{Name: l[q1+1 : q2], Ping: ping})
	}
	return info, players, true
}

// q3GetStatus queries a server, returns its players.
func q3GetStatus(addr string) ([]q3Player, bool) {
	_, players, ok := q3GetStatusInfo(addr)
	return players, ok
}

func serverAlive(addr string) (int, bool) {
	for attempt := 0; attempt < 2; attempt++ {
		if p, ok := q3GetStatus(addr); ok {
			return len(p), true
		}
	}
	return 0, false
}

// pickServer: first reachable (and non-empty, if skip_empty) server.
func pickServer(servers []string) string {
	var fallback string
	for _, s := range servers {
		count, alive := serverAlive(s)
		if !alive {
			log.Println("check:", s, "-> unreachable")
			continue
		}
		log.Println("check:", s, "-> alive,", count, "players")
		if cfg.SkipEmpty && count == 0 {
			if fallback == "" {
				fallback = s
			}
			continue
		}
		return s
	}
	return fallback
}

// chooseTarget picks a random follow target: humans (ping>0) preferred,
// excluding ourselves and (if possible) the current target.
func chooseTarget(addr, self, current string) (raw string, ping int, ok bool) {
	var players []q3Player
	var alive bool
	// retry: many servers rate-limit getstatus per IP (flood protection)
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(1500 * time.Millisecond)
		}
		players, alive = q3GetStatus(addr)
		if alive && len(players) > 0 {
			break
		}
	}
	if !alive || len(players) == 0 {
		return "", 0, false
	}
	selfC, curC := strings.ToLower(cleanName(self)), strings.ToLower(cleanName(current))
	humans := 0
	for _, p := range players {
		if p.Ping > 0 {
			humans++
		}
	}
	var pool []q3Player
	for _, p := range players {
		c := strings.ToLower(cleanName(p.Name))
		if c == selfC {
			continue
		}
		if cfg.PreferHumans && humans > 0 && p.Ping == 0 {
			continue
		}
		pool = append(pool, p)
	}
	if len(pool) == 0 {
		return "", 0, false
	}
	pick := pool[rng.Intn(len(pool))]
	if len(pool) > 1 {
		for guard := 0; guard < 8 && strings.ToLower(cleanName(pick.Name)) == curC; guard++ {
			pick = pool[rng.Intn(len(pool))]
		}
	}
	return pick.Name, pick.Ping, true
}

func nameOnServer(addr, name string) bool {
	players, ok := q3GetStatus(addr)
	if !ok {
		return false
	}
	want := strings.ToLower(cleanName(name))
	for _, p := range players {
		if strings.Contains(strings.ToLower(cleanName(p.Name)), want) {
			return true
		}
	}
	return false
}
