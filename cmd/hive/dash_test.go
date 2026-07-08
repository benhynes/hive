package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benhynes/hive/internal/client"
)

func alive(name string) client.AgentInfo {
	return client.AgentInfo{Agent: name, Alive: true, Controllable: true}
}

func pane(screen string, changedAgo time.Duration) *paneState {
	now := time.Now()
	return &paneState{Screen: screen, FetchedAt: now, ChangedAt: now.Add(-changedAgo)}
}

func TestStatusOf(t *testing.T) {
	now := time.Now()
	claudeWorking := "✻ Cogitating… (esc to interrupt)\n"
	claudePermission := "Do you want to proceed?\n ❯ 1. Yes\n   2. No\n"
	claudeTrust := "Do you trust the files in this folder?\n ❯ 1. Yes, proceed\n"
	claudeBypassWarn := "WARNING: Bypass Permissions mode\n ❯ 1. No, exit\n   2. Yes, I accept\n"
	claudeIdle := "❯ \n\n⏵⏵ bypass permissions on (shift+tab to cycle)\n"
	claudeIdle2 := "❯ \n\n? for shortcuts\n"
	notLoggedIn := "Not logged in · /login\n"

	cases := []struct {
		name string
		a    client.AgentInfo
		p    *paneState
		want string
	}{
		{"dead", client.AgentInfo{Agent: "x@h", Alive: false, Controllable: true}, nil, "dead"},
		{"uncontrolled", client.AgentInfo{Agent: "x@h", Alive: true}, nil, "uncontrolled"},
		{"pending", alive("x@h"), nil, "pending"},
		{"unreachable", alive("x@h"), &paneState{Err: "host is down", FetchedAt: now}, "unreachable"},
		{"gone-is-dead", alive("x@h"), &paneState{Err: "agent x is gone (pane closed or process replaced)", FetchedAt: now}, "dead"},
		{"working", alive("x@h"), pane(claudeWorking, 0), "working"},
		{"permission", alive("x@h"), pane(claudePermission, time.Minute), "attention"},
		{"trust", alive("x@h"), pane(claudeTrust, time.Minute), "attention"},
		{"bypass-warning", alive("x@h"), pane(claudeBypassWarn, time.Minute), "attention"},
		{"not-logged-in", alive("x@h"), pane(notLoggedIn, time.Minute), "attention"},
		{"idle-bypass-banner", alive("x@h"), pane(claudeIdle, time.Minute), "idle"},
		// Working spinner with the "esc to interrupt" hint truncated away
		// by a narrow pane, idle banner still visible below — must read
		// as working, not idle.
		{"working-truncated-hint", alive("x@h"),
			pane("✻ Orbiting… (8m 17s · ↓ 31.7k tokens)\n\n⏵⏵ bypass permissions on (shift+tab to cycle) · esc to \n", 0), "working"},
		{"done-spinner-is-idle", alive("x@h"),
			pane("✻ Brewed for 37s\n\n❯ \n? for shortcuts\n", time.Minute), "idle"},
		{"idle-shortcuts", alive("x@h"), pane(claudeIdle2, time.Minute), "idle"},
		{"active", alive("x@h"), pane("compiling...\n", 3*time.Second), "active"},
		{"quiet", alive("x@h"), pane("compiling...\n", time.Minute), "quiet"},
	}
	for _, c := range cases {
		if got := statusOf(c.a, c.p, now); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// The idle banner "bypass permissions on" must not trip the "bypass
// permissions mode" WARNING-dialog pattern.
func TestStatusIdleBannerNotAttention(t *testing.T) {
	p := pane("❯ \n⏵⏵ bypass permissions on (shift+tab to cycle)\n", time.Minute)
	if got := statusOf(alive("x@h"), p, time.Now()); got != "idle" {
		t.Fatalf("idle banner misread as %q", got)
	}
}

// Working beats a stale dialog string higher up the scrollback tail.
func TestStatusWorkingWins(t *testing.T) {
	p := pane("Do you want to proceed?\n(earlier)\n✻ Running… (esc to interrupt)\n", 0)
	if got := statusOf(alive("x@h"), p, time.Now()); got != "working" {
		t.Fatalf("got %q want working", got)
	}
}

func TestLastLines(t *testing.T) {
	if got := lastLines("a\nb\nc\n\n\n", 2); got != "b\nc" {
		t.Fatalf("got %q", got)
	}
	if got := lastLines("a\nb", 5); got != "a\nb" {
		t.Fatalf("got %q", got)
	}
}

func newTestDash(t *testing.T) *dash {
	t.Helper()
	c := &client.Client{Net: "testnet"}
	d := newDash(c, 0)
	d.agents = []client.AgentInfo{alive("bob@h1")}
	d.panes["bob@h1"] = pane("hello\n❯ \n? for shortcuts\n", time.Minute)
	return d
}

func TestDashAPIAuth(t *testing.T) {
	d := newTestDash(t)
	h := d.handler("127.0.0.1:7780")

	// Page serves without token, with the token substituted in.
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "127.0.0.1:7780"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 || !strings.Contains(rw.Body.String(), d.token) {
		t.Fatalf("page: code=%d tokenIncluded=%v", rw.Code, strings.Contains(rw.Body.String(), d.token))
	}
	if strings.Contains(rw.Body.String(), "{{TOKEN}}") || strings.Contains(rw.Body.String(), "{{NET}}") {
		t.Fatal("placeholders not substituted")
	}

	// API without token → 401.
	req = httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:7780"
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d want 401", rw.Code)
	}

	// API with token → 200 and sane JSON.
	req = httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "localhost:7780" // loopback alias allowed
	req.Header.Set("X-Dash-Token", d.token)
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("with token: got %d", rw.Code)
	}
	var out struct {
		Net    string      `json:"net"`
		Agents []dashAgent `json:"agents"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Net != "testnet" || len(out.Agents) != 1 || out.Agents[0].Status != "idle" {
		t.Fatalf("state = %+v", out)
	}

	// DNS-rebinding Host → 403 even with the token.
	req = httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "evil.example:7780"
	req.Header.Set("X-Dash-Token", d.token)
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Fatalf("rebinding host: got %d want 403", rw.Code)
	}
}
