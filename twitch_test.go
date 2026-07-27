package main

import (
	"strings"
	"testing"
	"time"
)

/* -------- pure: title rendering, truncation, vars, token -------- */

func TestRenderTitleAllPlaceholders(t *testing.T) {
	tmpl := "Wolffiles.eu 24/7 ET | {map} @ {server} | {mod} {players}p | /connect {serverip}"
	got := renderTitle(tmpl, map[string]string{
		"map": "goldrush", "server": "Wolf Server", "serverip": "1.2.3.4:27960",
		"mod": "silent", "players": "12",
	})
	want := "Wolffiles.eu 24/7 ET | goldrush @ Wolf Server | silent 12p | /connect 1.2.3.4:27960"
	if got != want {
		t.Errorf("render = %q\nwant   %q", got, want)
	}
}

func TestRenderTitleUnknownAndEmpty(t *testing.T) {
	// unknown token -> empty (not the literal); empty known placeholder collapses.
	got := renderTitle("{map} @ {server} {bogus}", map[string]string{"map": "supply"})
	if got != "supply @" {
		t.Errorf("unknown/empty handling = %q, want %q", got, "supply @")
	}
	if strings.Contains(got, "{") {
		t.Errorf("literal token leaked: %q", got)
	}
}

func TestTwitchTitleVarsStripsColour(t *testing.T) {
	vars := twitchTitleVars("goldrush", "^1Wolf^7files ^3ET", "silent", "1.2.3.4:27960", 8)
	if vars["server"] != "Wolffiles ET" {
		t.Errorf("colour codes not stripped from {server}: %q", vars["server"])
	}
	if vars["players"] != "8" {
		t.Errorf("players = %q, want 8", vars["players"])
	}
}

func TestRenderTitleTruncatesOnWordBoundary(t *testing.T) {
	long := strings.Repeat("word ", 40) // 200 chars
	got := renderTitle("{x}", map[string]string{"x": strings.TrimSpace(long)})
	if len([]rune(got)) > maxTitleLen {
		t.Fatalf("not truncated to %d: len=%d", maxTitleLen, len([]rune(got)))
	}
	if strings.HasSuffix(got, "wor") || strings.HasSuffix(got, " ") {
		t.Errorf("truncation not on a word boundary: %q", got)
	}
}

func TestRenderTitleDropsConnectTailWholeIP(t *testing.T) {
	// a long hostname pushes the ip past 140: the WHOLE "/connect <ip>" tail must
	// be dropped, never a half-written address.
	host := strings.Repeat("A", 120)
	tmpl := "{server} | /connect {serverip}"
	ip := "203.0.113.42:27960"
	got := renderTitle(tmpl, map[string]string{"server": host, "serverip": ip})
	if len([]rune(got)) > maxTitleLen {
		t.Fatalf("over cap: %d", len([]rune(got)))
	}
	if strings.Contains(got, "/connect") || strings.Contains(got, "203.0") {
		t.Errorf("connect/ip tail should be dropped whole, got %q", got)
	}
	if strings.Contains(got, ":2796") { // no partial address
		t.Errorf("half-written ip leaked: %q", got)
	}
}

func TestDropConnectTail(t *testing.T) {
	s := "Title here | /connect 1.2.3.4:27960"
	// ip fits within a generous cap -> not dropped
	if _, ok := dropConnectTail(s, "1.2.3.4:27960", 140); ok {
		t.Error("ip that fits must not be dropped")
	}
	// tiny cap so the ip can't fit -> dropped whole, no dangling separator
	got, ok := dropConnectTail(s, "1.2.3.4:27960", 15)
	if !ok || strings.Contains(got, "/connect") || strings.Contains(got, "|") {
		t.Errorf("dropConnectTail = %q ok=%v", got, ok)
	}
}

func TestTokenNeedsRefresh(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	buf := 5 * time.Minute
	if !tokenNeedsRefresh(time.Time{}, base, buf) {
		t.Error("zero expiry (no token yet) must need refresh")
	}
	if !tokenNeedsRefresh(base.Add(2*time.Minute), base, buf) {
		t.Error("expiry within buffer must need refresh")
	}
	if tokenNeedsRefresh(base.Add(time.Hour), base, buf) {
		t.Error("expiry far out must NOT need refresh")
	}
}

/* -------- client logic with a fake transport -------- */

type fakeTransport struct {
	refreshCalls int
	userCalls    int
	titleCalls   []string
	markerCalls  []string
	liveResult   bool
	refreshErr   error
	newToken     string
	expiresSec   int
	bid          string
	unauth401    int // return 401 from setTitle this many times, then succeed
}

func (f *fakeTransport) isLive(id, tok, bid string) (bool, error) { return f.liveResult, nil }
func (f *fakeTransport) marker(id, tok, bid, desc string) error {
	f.markerCalls = append(f.markerCalls, desc)
	return nil
}

func (f *fakeTransport) refresh(id, sec, rt string) (string, string, int, error) {
	f.refreshCalls++
	if f.refreshErr != nil {
		return "", "", 0, f.refreshErr
	}
	if f.newToken == "" {
		f.newToken = "tok"
	}
	if f.expiresSec == 0 {
		f.expiresSec = 3600
	}
	return f.newToken, "", f.expiresSec, nil
}
func (f *fakeTransport) userID(id, tok string) (string, error) {
	f.userCalls++
	if f.bid == "" {
		f.bid = "12345"
	}
	return f.bid, nil
}
func (f *fakeTransport) setTitle(id, tok, bid, title string) error {
	if f.unauth401 > 0 {
		f.unauth401--
		return errTwitchUnauthorized
	}
	f.titleCalls = append(f.titleCalls, title)
	return nil
}

func newTestTwitch(f *fakeTransport) *twitchClient {
	base := time.Unix(1_000_000, 0)
	return &twitchClient{
		tr: f, clientID: "c", secret: "s", refreshToken: "r",
		broadcasterID: "12345",
		accessToken:   "tok", tokenExpiry: base.Add(time.Hour),
		now: func() time.Time { return base },
	}
}

func TestTwitchSetTitleChangeGate(t *testing.T) {
	f := &fakeTransport{}
	tc := newTestTwitch(f)
	tc.setTitle("Title A")
	tc.setTitle("Title A") // unchanged -> no second call
	tc.setTitle("Title B")
	if len(f.titleCalls) != 2 {
		t.Fatalf("expected 2 title calls (A, B), got %v", f.titleCalls)
	}
	if f.titleCalls[0] != "Title A" || f.titleCalls[1] != "Title B" {
		t.Errorf("unexpected titles: %v", f.titleCalls)
	}
	// empty title never calls
	tc.setTitle("")
	if len(f.titleCalls) != 2 {
		t.Errorf("empty title must not call Twitch")
	}
}

func TestTwitch401RefreshesOnceAndRetries(t *testing.T) {
	f := &fakeTransport{unauth401: 1} // first setTitle 401s, then succeeds
	tc := newTestTwitch(f)
	tc.setTitle("Title")
	if f.refreshCalls != 1 {
		t.Errorf("a 401 must force exactly one refresh, got %d", f.refreshCalls)
	}
	if len(f.titleCalls) != 1 || f.titleCalls[0] != "Title" {
		t.Errorf("title should have been set on retry, got %v", f.titleCalls)
	}
}

func TestTwitchRefreshRejectedDisables(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	f := &fakeTransport{refreshErr: errTwitchRefreshRejected}
	tc := &twitchClient{tr: f, clientID: "c", secret: "s", refreshToken: "bad",
		broadcasterID: "12345", now: func() time.Time { return base }} // no token -> must refresh
	tc.setTitle("Title")
	if !tc.disabled {
		t.Fatal("a rejected refresh token must disable Twitch")
	}
	// once disabled, no further Twitch calls and refresh isn't hammered
	before := f.refreshCalls
	tc.setTitle("Another")
	if f.refreshCalls != before {
		t.Errorf("disabled client must not keep calling refresh")
	}
	if len(f.titleCalls) != 0 {
		t.Errorf("disabled client must not set titles")
	}
}

/* -------- Part 2: markers -------- */

func TestMarkerAllowedRateLimit(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	min := 8 * time.Second
	if !markerAllowed(time.Time{}, base, min) {
		t.Error("first marker (no prior) must be allowed")
	}
	if markerAllowed(base, base.Add(3*time.Second), min) {
		t.Error("a marker 3s after the last must be rate-limited")
	}
	if !markerAllowed(base, base.Add(9*time.Second), min) {
		t.Error("a marker 9s after the last must be allowed")
	}
}

func TestMarkerDescription(t *testing.T) {
	if got := markerDescription("Replay", "Triple kill by Rambo", "goldrush"); got != "Replay: Triple kill by Rambo on goldrush" {
		t.Errorf("desc = %q", got)
	}
	if got := markerDescription("Replay", "", "supply"); got != "Replay on supply" {
		t.Errorf("no-label desc = %q", got)
	}
	// capped at 140 on a word boundary
	long := markerDescription("Replay", strings.Repeat("word ", 40), "map")
	if len([]rune(long)) > maxMarkerLen {
		t.Errorf("marker not capped: %d", len([]rune(long)))
	}
}

func TestTwitchMarkerSkipsWhenNotLive(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.TwitchMarkersEnabled = true
	f := &fakeTransport{liveResult: false}
	tc := newTestTwitch(f)
	tc.marker("Replay: X on goldrush")
	if len(f.markerCalls) != 0 {
		t.Errorf("marker must be skipped when the stream is not live, got %v", f.markerCalls)
	}
}

func TestTwitchMarkerRateLimited(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.TwitchMarkersEnabled = true

	base := time.Unix(1_000_000, 0)
	clk := base
	f := &fakeTransport{liveResult: true}
	tc := newTestTwitch(f)
	tc.now = func() time.Time { return clk }

	tc.marker("first")            // allowed
	tc.marker("second, too soon") // rate-limited (same instant)
	clk = base.Add(10 * time.Second)
	tc.marker("third, after gap") // allowed again
	if len(f.markerCalls) != 2 || f.markerCalls[0] != "first" || f.markerCalls[1] != "third, after gap" {
		t.Errorf("rate-limit wrong, got %v", f.markerCalls)
	}
}

func TestTwitchMarkerDisabledByFlag(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.TwitchMarkersEnabled = false // markers off
	f := &fakeTransport{liveResult: true}
	tc := newTestTwitch(f)
	tc.marker("Replay: X")
	if len(f.markerCalls) != 0 {
		t.Errorf("markers disabled: must not call Twitch, got %v", f.markerCalls)
	}
}

func TestTwitchTitleKeyChangedDedupes(t *testing.T) {
	tc := newTestTwitch(&fakeTransport{})
	if !tc.titleKeyChanged("goldrush|1.2.3.4:27960") {
		t.Error("first key must be treated as changed")
	}
	if tc.titleKeyChanged("goldrush|1.2.3.4:27960") {
		t.Error("same key must dedupe")
	}
	if !tc.titleKeyChanged("supply|1.2.3.4:27960") {
		t.Error("new map must be treated as changed")
	}
	tc.disabled = true
	if tc.titleKeyChanged("radar|5.6.7.8:27960") {
		t.Error("disabled client must not act")
	}
}
