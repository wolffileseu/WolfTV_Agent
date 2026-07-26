package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Twitch integration: set the channel title on server switch (Part 1) and drop
// stream markers on replays/highlights (Part 2). Inert unless twitch_enabled.
//
// SAFETY (same invariant as replay): a Twitch failure must never disturb the
// broadcast or block a server switch. Every call runs in a goroutine off the
// hot path and only logs on error; a rejected refresh token disables Twitch for
// the session instead of hammering the endpoint.

const (
	maxTitleLen        = 140
	tokenRefreshBuffer = 5 * time.Minute // refresh this long before the token expires
)

/* ----------------------------- pure helpers ------------------------------ */

var placeholderRE = regexp.MustCompile(`\{([a-zA-Z_]+)\}`)

// substitutePlaceholders replaces {key} with vars[key]; an unknown or unset key
// becomes empty (never the literal token).
func substitutePlaceholders(tmpl string, vars map[string]string) string {
	return placeholderRE.ReplaceAllStringFunc(tmpl, func(m string) string {
		return vars[m[1:len(m)-1]] // "" if absent
	})
}

// normalizeTitleSpaces collapses whitespace runs (an empty placeholder leaves a
// double space) and trims. Titles are single-line.
func normalizeTitleSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateWordBoundary cuts s to at most max runes on a word boundary.
func truncateWordBoundary(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	cut := string([]rune(s)[:max])
	if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ")
}

// dropConnectTail removes a trailing "/connect <ip>" tail when the ip would not
// fully fit in max runes, so a truncated title never shows a half-written
// address. Returns (title-without-tail, true) only when it actually dropped it.
func dropConnectTail(s, ip string, max int) (string, bool) {
	idx := strings.LastIndex(s, ip)
	if idx < 0 {
		return "", false
	}
	ipEndRune := utf8.RuneCountInString(s[:idx]) + utf8.RuneCountInString(ip)
	if ipEndRune <= max {
		return "", false // the ip fits -- no need to drop it
	}
	head := strings.TrimRight(s[:idx], " ")
	head = strings.TrimSuffix(head, "/connect")
	head = strings.TrimRight(head, " |")
	return head, true
}

// renderTitle fills the template, collapses spaces, and fits it into 140 runes:
// it prefers dropping the whole "/connect {serverip}" tail (so the address is
// never cut mid-way), then falls back to a plain word-boundary truncation.
func renderTitle(tmpl string, vars map[string]string) string {
	s := normalizeTitleSpaces(substitutePlaceholders(tmpl, vars))
	if utf8.RuneCountInString(s) <= maxTitleLen {
		return s
	}
	if ip := vars["serverip"]; ip != "" {
		if cut, ok := dropConnectTail(s, ip, maxTitleLen); ok {
			s = normalizeTitleSpaces(cut)
			if utf8.RuneCountInString(s) <= maxTitleLen {
				return s
			}
		}
	}
	return truncateWordBoundary(s, maxTitleLen)
}

// tokenNeedsRefresh reports whether the access token should be refreshed now
// (expired, within the buffer, or never obtained). Pure.
func tokenNeedsRefresh(expiry, now time.Time, buffer time.Duration) bool {
	return now.After(expiry.Add(-buffer))
}

/* ------------------------------- transport ------------------------------- */

// Sentinel errors let the client distinguish "access token stale, refresh and
// retry" from "refresh token dead, disable and tell the operator".
var (
	errTwitchUnauthorized    = errStr("twitch: unauthorized (401)")
	errTwitchRefreshRejected = errStr("twitch: refresh token rejected")
)

// twitchTransport isolates the Helix/OAuth HTTP calls so the client logic can be
// unit-tested with a fake. The real implementation is httpTransport.
type twitchTransport interface {
	// refresh exchanges the refresh token for an access token; newRefresh is the
	// (possibly rotated) refresh token, "" if unchanged.
	refresh(clientID, secret, refreshToken string) (accessToken, newRefresh string, expiresSec int, err error)
	// userID resolves the broadcaster id of the token's user.
	userID(clientID, accessToken string) (string, error)
	// setTitle PATCHes the channel title.
	setTitle(clientID, accessToken, broadcasterID, title string) error
}

// httpTransport is the real Twitch client. Cannot be unit-tested here (needs the
// live endpoints); it is exercised by a live run.
type httpTransport struct{ hc *http.Client }

func newHTTPTransport() *httpTransport {
	return &httpTransport{hc: &http.Client{Timeout: 10 * time.Second}}
}

func (h *httpTransport) refresh(clientID, secret, refreshToken string) (string, string, int, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {secret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	req, _ := http.NewRequest("POST", "https://id.twitch.tv/oauth2/token",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.hc.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		return "", "", 0, errTwitchRefreshRejected
	}
	if resp.StatusCode != 200 {
		return "", "", 0, errStr("twitch refresh: status " + resp.Status)
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", "", 0, err
	}
	return r.AccessToken, r.RefreshToken, r.ExpiresIn, nil
}

// helixReq builds an authenticated Helix request.
func helixReq(method, url, clientID, accessToken string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Client-Id", clientID)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (h *httpTransport) userID(clientID, accessToken string) (string, error) {
	req, err := helixReq("GET", "https://api.twitch.tv/helix/users", clientID, accessToken, nil)
	if err != nil {
		return "", err
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return "", errTwitchUnauthorized
	}
	if resp.StatusCode != 200 {
		return "", errStr("twitch users: status " + resp.Status)
	}
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if len(r.Data) == 0 {
		return "", errStr("twitch users: empty response")
	}
	return r.Data[0].ID, nil
}

func (h *httpTransport) setTitle(clientID, accessToken, broadcasterID, title string) error {
	body, _ := json.Marshal(map[string]string{"title": title})
	req, err := helixReq("PATCH",
		"https://api.twitch.tv/helix/channels?broadcaster_id="+url.QueryEscape(broadcasterID),
		clientID, accessToken, body)
	if err != nil {
		return err
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return errTwitchUnauthorized
	}
	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return errStr("twitch set title: status " + resp.Status + " " + string(b))
	}
	return nil
}

/* -------------------------------- client --------------------------------- */

type twitchClient struct {
	mu sync.Mutex
	tr twitchTransport

	clientID, secret string
	refreshToken     string
	broadcasterID    string

	accessToken string
	tokenExpiry time.Time

	disabled bool

	// title state
	lastTitleKey string // "map|serverip" -- cheap dedupe so we don't getstatus every tick
	currentTitle string // last title we successfully set (for /twitch/status)

	now func() time.Time // injectable clock for tests
}

// twitch is the live client, nil unless twitch_enabled.
var twitch *twitchClient

func (t *twitchClient) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *twitchClient) disableLocked(reason string) {
	if t.disabled {
		return
	}
	t.disabled = true
	log.Println("twitch: DISABLED for this session --", reason)
}

// ensureTokenLocked refreshes the access token if it is missing/near expiry. A
// rejected refresh token disables Twitch (one clear message) rather than retrying.
func (t *twitchClient) ensureTokenLocked() error {
	if t.disabled {
		return errStr("twitch disabled")
	}
	if !tokenNeedsRefresh(t.tokenExpiry, t.clock(), tokenRefreshBuffer) {
		return nil
	}
	at, nr, exp, err := t.tr.refresh(t.clientID, t.secret, t.refreshToken)
	if err != nil {
		if err == errTwitchRefreshRejected {
			t.disableLocked("refresh token rejected (revoked or bad client secret) -- " +
				"re-authorize per the README and restart")
		}
		return err
	}
	t.accessToken = at
	if nr != "" {
		t.refreshToken = nr // Twitch may rotate it (in-memory only)
	}
	t.tokenExpiry = t.clock().Add(time.Duration(exp) * time.Second)
	return nil
}

func (t *twitchClient) ensureBroadcasterLocked() error {
	if t.broadcasterID != "" {
		return nil
	}
	id, err := t.tr.userID(t.clientID, t.accessToken)
	if err != nil {
		return err
	}
	if id == "" {
		return errStr("twitch: empty broadcaster id")
	}
	t.broadcasterID = id
	log.Println("twitch: broadcaster id resolved:", id)
	return nil
}

// doHelixLocked runs fn with a valid token + broadcaster id, refreshing once and
// retrying if fn reports a 401 (stale access token).
func (t *twitchClient) doHelixLocked(fn func(token, bid string) error) error {
	if err := t.ensureTokenLocked(); err != nil {
		return err
	}
	if err := t.ensureBroadcasterLocked(); err != nil {
		return err
	}
	err := fn(t.accessToken, t.broadcasterID)
	if err == errTwitchUnauthorized {
		t.tokenExpiry = time.Time{} // force a refresh
		if rerr := t.ensureTokenLocked(); rerr != nil {
			return rerr
		}
		err = fn(t.accessToken, t.broadcasterID)
	}
	return err
}

// init does the startup auth + broadcaster resolve (best-effort; logs and
// returns on failure). Runs in a goroutine from main.
func (t *twitchClient) init() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureTokenLocked(); err != nil {
		log.Println("twitch: initial auth failed:", err)
		return
	}
	if err := t.ensureBroadcasterLocked(); err != nil {
		log.Println("twitch: broadcaster resolve failed:", err)
		return
	}
	log.Println("twitch: authorized, broadcaster", t.broadcasterID)
}

// titleKeyChanged reports whether the (map, server) pair differs from the last
// one we acted on -- a cheap gate so a getstatus + render only happens on an
// actual server/map change, not every telemetry tick.
func (t *twitchClient) titleKeyChanged(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.disabled || key == t.lastTitleKey {
		return false
	}
	t.lastTitleKey = key
	return true
}

// setTitle updates the channel title iff it actually changed. Runs Twitch I/O
// under the lock (fine -- it is called from a goroutine, not the hot path).
func (t *twitchClient) setTitle(title string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.disabled || title == "" || title == t.currentTitle {
		return
	}
	err := t.doHelixLocked(func(tok, bid string) error {
		return t.tr.setTitle(t.clientID, tok, bid, title)
	})
	if err != nil {
		log.Println("twitch: set title failed:", err)
		return
	}
	t.currentTitle = title
	log.Println("twitch: title ->", title)
}

// twitchTitleVars builds the placeholder map. {server} is the hostname with
// colour codes stripped; unset facts are empty. Pure, so it is unit-tested.
func twitchTitleVars(mapName, hostname, mod, serverIP string, players int) map[string]string {
	pl := ""
	if players >= 0 {
		pl = strconv.Itoa(players)
	}
	return map[string]string{
		"map":      mapName,
		"server":   cleanName(hostname), // strip ^x colour codes
		"serverip": serverIP,
		"mod":      mod,
		"players":  pl,
	}
}

// updateTitleFor gathers the live server facts (hostname/mod/player-count via
// getstatus) and sets the rendered title. Runs in a goroutine.
func (t *twitchClient) updateTitleFor(mapName, serverIP string) {
	host, mod, players := "", "", -1
	if info, pl, ok := q3GetStatusInfo(serverIP); ok {
		host = info["sv_hostname"]
		mod = info["gamename"]
		players = len(pl)
	}
	t.setTitle(renderTitle(cfg.TwitchTitleTemplate, twitchTitleVars(mapName, host, mod, serverIP, players)))
}

// twitchOnTelemetry is the hook called from the live pipeline on every status
// update. It fires a title update only once the instance has settled on a map
// and only when the map/server actually changed. No-op unless Twitch is on.
func twitchOnTelemetry(state, mapName, serverIP string) {
	if twitch == nil || state != "active" || mapName == "" || serverIP == "" {
		return
	}
	key := mapName + "|" + serverIP
	if !twitch.titleKeyChanged(key) {
		return
	}
	go twitch.updateTitleFor(mapName, serverIP)
}

/* -------------------------------- status --------------------------------- */

func (t *twitchClient) status() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return map[string]any{
		"enabled":       true,
		"authorized":    !t.disabled && t.accessToken != "",
		"broadcaster":   t.broadcasterID,
		"current_title": t.currentTitle,
	}
}

// GET /twitch/status
func handleTwitchStatus(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	if twitch == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, 200, twitch.status())
}

// setupTwitch builds the client from config and kicks off startup auth. Called
// from main; a no-op unless twitch_enabled and the credentials are present.
func setupTwitch() {
	if !cfg.TwitchEnabled {
		return
	}
	if cfg.TwitchClientID == "" || cfg.TwitchClientSecret == "" || cfg.TwitchRefreshToken == "" {
		log.Println("twitch: enabled but client_id/client_secret/refresh_token incomplete -- disabled")
		return
	}
	twitch = &twitchClient{
		tr:            newHTTPTransport(),
		clientID:      cfg.TwitchClientID,
		secret:        cfg.TwitchClientSecret,
		refreshToken:  cfg.TwitchRefreshToken,
		broadcasterID: cfg.TwitchBroadcasterID, // "" -> resolved on first use
	}
	log.Println("twitch: enabled")
	go twitch.init()
}
