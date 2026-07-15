// Latency profile of the message path. This is a measurement harness, not an
// assertion suite: it prints where the time actually goes on one host so that
// "is this fast?" is answered with numbers instead of intuition.
//
//	go test ./cmd/hive/ -run TestLatencyProfile -v
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	i := int(float64(len(d)-1) * p)
	return d[i]
}

func report(t *testing.T, label string, ds []time.Duration) {
	t.Helper()
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	t.Logf("%-42s n=%3d  p50=%-10v p90=%-10v mean=%v",
		label, len(ds), pct(ds, 0.50), pct(ds, 0.90), sum/time.Duration(len(ds)))
}

func TestLatencyProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("perf harness")
	}
	h := startHub(t, "perfhost")
	out := mustCLI(t, h.env(), "net", "create", "dev")
	for _, m := range regexpTok.FindAllStringSubmatch(out, -1) {
		if m[1] == "msg" {
			h.msgTok = m[2]
		} else {
			h.ctlTok = m[2]
		}
	}
	aliceEnv := register(t, h, "alice")
	register(t, h, "bob")

	tok := func(env []string, key string) string {
		for _, e := range env {
			if strings.HasPrefix(e, key+"=") {
				return strings.TrimPrefix(e, key+"=")
			}
		}
		return ""
	}
	aliceTok := tok(aliceEnv, "HIVE_TOKEN")
	sendURL := h.url() + "/v1/nets/dev/send"

	post := func(url, token, body string) {
		req, _ := http.NewRequest("POST", url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// (1) The wire itself: one POST /send on a warm keep-alive connection.
	// This is the whole hub-internal cost — validate, stamp, append, fsync.
	var wire []time.Duration
	for i := 0; i < 200; i++ {
		body := fmt.Sprintf(`{"to":"bob@perfhost","kind":"msg","body":"m%d"}`, i)
		st := time.Now()
		post(sendURL, aliceTok, body)
		wire = append(wire, time.Since(st))
	}
	report(t, "1. POST /send (warm conn, hub-internal)", wire)

	// (2) The path that actually matters when an agent is WAITING for work:
	// sender POSTs, and a long-poll already blocked in GET /inbox wakes.
	// This is the floor for agent-to-agent handoff.
	carolTok := tok(register(t, h, "carol"), "HIVE_TOKEN")
	var wake []time.Duration
	var after int64 // advance the cursor by hand, or the next poll returns the
	// previous message instantly and we measure nothing.
	for i := 0; i < 50; i++ {
		type res struct {
			at   time.Time
			top  int64
			msgs int
		}
		got := make(chan res, 1)
		go func(after int64) {
			url := fmt.Sprintf("%s/v1/nets/dev/inbox?wait=25&max=10&after=%d", h.url(), after)
			req, _ := http.NewRequest("GET", url, nil)
			req.Header.Set("Authorization", "Bearer "+carolTok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				got <- res{}
				return
			}
			var rr struct {
				Msgs []struct {
					Seq int64 `json:"seq"`
				} `json:"msgs"`
			}
			json.NewDecoder(resp.Body).Decode(&rr)
			resp.Body.Close()
			r := res{at: time.Now(), msgs: len(rr.Msgs)}
			for _, m := range rr.Msgs {
				if m.Seq > r.top {
					r.top = m.Seq
				}
			}
			got <- r
		}(after)
		time.Sleep(30 * time.Millisecond) // let the long-poll actually block
		st := time.Now()
		post(sendURL, aliceTok, fmt.Sprintf(`{"to":"carol@perfhost","kind":"msg","body":"w%d"}`, i))
		r := <-got
		if r.msgs == 0 {
			t.Fatalf("iter %d: long-poll returned no message", i)
		}
		if d := r.at.Sub(st); d < 0 {
			t.Fatalf("iter %d: long-poll returned before the send (%v) — cursor not advancing", i, d)
		} else {
			wake = append(wake, d)
		}
		after = r.top
	}
	report(t, "2. send -> blocked long-poll wakes (E2E)", wake)

	// (3) What an agent actually pays now: the MCP server process is already
	// up, so a send is one JSON-RPC round trip over an open pipe. (The old
	// `hive send` fork+exec path — ~8ms per message — no longer exists.)
	sess := startMCP(t, aliceEnv)
	var mcpSend []time.Duration
	for i := 0; i < 50; i++ {
		st := time.Now()
		sess.mustCall("hive_send", map[string]any{"to": "bob", "body": "hello"})
		mcpSend = append(mcpSend, time.Since(st))
	}
	report(t, "3. hive_send via MCP (warm process)", mcpSend)
}

var regexpTok = regexp.MustCompile(`(?m)^  (msg|control) token: +([0-9a-f]{64})$`)
