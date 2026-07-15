package hub_test

// Regression tests for the multi-agent review findings: the register
// TOCTOU, broadcast excluding an agent named "human", client Asks/Answer
// pagination past the 500-per-read cap, and Deregister token selection.
// These run against the real HTTP handler; no tmux needed (agents are
// registered without panes).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/benhynes/hive/internal/client"
	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/hub"
	"github.com/benhynes/hive/internal/proto"
)

func newTestNet(t *testing.T) (*httptest.Server, config.NetConfig) {
	t.Helper()
	t.Setenv("HIVE_HOME", t.TempDir())
	nc := config.NetConfig{
		Name: "dev", MsgToken: proto.NewToken(), ControlToken: proto.NewToken(),
		Hosts: map[string]string{"testhost": "127.0.0.1:1"},
	}
	if err := config.SaveNet(nc); err != nil {
		t.Fatal(err)
	}
	h := hub.New(config.Config{HostName: "testhost", Bind: "127.0.0.1", Port: 1})
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return srv, nc
}

func call(t *testing.T, srv *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func registerHTTP(t *testing.T, srv *httptest.Server, nc config.NetConfig, name string) string {
	t.Helper()
	code, out := call(t, srv, "POST", "/v1/nets/dev/register", nc.MsgToken, map[string]any{"name": name})
	if code != 200 {
		t.Fatalf("register %s: %d %v", name, code, out)
	}
	return out["token"].(string)
}

func agentClient(t *testing.T, srv *httptest.Server, tok, control string) *client.Client {
	t.Helper()
	t.Setenv("HIVE_ADDR", srv.URL)
	t.Setenv("HIVE_NET", "dev")
	t.Setenv("HIVE_TOKEN", tok)
	t.Setenv("HIVE_CONTROL_TOKEN", control)
	c, err := client.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Two registrations of the same name racing through the check-then-Put
// window must produce exactly one winner, and the winner's token must
// keep resolving (the loser's Put used to silently revoke it).
func TestConcurrentRegisterSingleWinner(t *testing.T) {
	srv, nc := newTestNet(t)
	const claimants = 32
	var (
		wg   sync.WaitGroup
		oks  atomic.Int32
		toks [claimants]string
	)
	for i := 0; i < claimants; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// pid binding makes the handler run `ps` between the
			// taken-check and the Put, widening the race window.
			code, out := call(t, srv, "POST", "/v1/nets/dev/register", nc.MsgToken,
				map[string]any{"name": "stormy", "pid": os.Getpid()})
			if code == 200 {
				oks.Add(1)
				toks[i], _ = out["token"].(string)
			}
		}(i)
	}
	wg.Wait()
	if got := oks.Load(); got != 1 {
		t.Fatalf("want exactly 1 winning registration, got %d", got)
	}
	for _, tok := range toks {
		if tok == "" {
			continue
		}
		if code, out := call(t, srv, "GET", "/v1/nets/dev/inbox", tok, nil); code != 200 {
			t.Fatalf("winner's token no longer resolves: %d %v", code, out)
		}
	}
}

// A net-token @all send stamps from="human@host"; a real agent named
// "human" must still receive it.
func TestBroadcastReachesAgentNamedHuman(t *testing.T) {
	srv, nc := newTestNet(t)
	registerHTTP(t, srv, nc, "human")
	code, out := call(t, srv, "POST", "/v1/nets/dev/send", nc.MsgToken,
		map[string]any{"to": "@all", "body": "hello"})
	if code != 200 {
		t.Fatalf("send: %d %v", code, out)
	}
	results := out["results"].(map[string]any)
	if results["human@testhost"] != "delivered" {
		t.Fatalf("agent named human missed the broadcast: %v", results)
	}
}

// Asks/Answer must see the whole 1000-message retained window even
// though one server read caps at 500 messages.
func TestAsksAnswerBeyond500(t *testing.T) {
	srv, nc := newTestNet(t)
	aliceTok := registerHTTP(t, srv, nc, "alice")
	bobTok := registerHTTP(t, srv, nc, "bob")

	for i := 0; i < 550; i++ {
		code, out := call(t, srv, "POST", "/v1/nets/dev/send", nc.MsgToken,
			map[string]any{"to": "bob@testhost", "body": fmt.Sprintf("filler %d", i)})
		if code != 200 || out["results"].(map[string]any)["bob@testhost"] != "delivered" {
			t.Fatalf("filler send %d: %d %v", i, code, out)
		}
	}
	code, out := call(t, srv, "POST", "/v1/nets/dev/send", aliceTok,
		map[string]any{"to": "bob@testhost", "kind": "ask", "body": "seen past 500?"})
	if code != 200 {
		t.Fatalf("ask send: %d %v", code, out)
	}
	askID := out["id"].(string)

	bob := agentClient(t, srv, bobTok, "")
	asks, err := bob.Asks()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range asks {
		if m.Env.ID == askID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ask at seq 551 not found by Asks (got %d asks)", len(asks))
	}
	res, err := bob.Answer(askID, "yes")
	if err != nil {
		t.Fatal(err)
	}
	if res.Results["alice@testhost"] != "delivered" {
		t.Fatalf("answer not delivered: %v", res.Results)
	}
	code, out = call(t, srv, "GET", "/v1/nets/dev/inbox?after=0&max=500", aliceTok, nil)
	if code != 200 {
		t.Fatalf("alice inbox: %d %v", code, out)
	}
	msgs := out["msgs"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("alice expected 1 message, got %d", len(msgs))
	}
	env := msgs[0].(map[string]any)["env"].(map[string]any)
	if env["kind"] != "answer" || env["corr_id"] != askID {
		t.Fatalf("alice's answer wrong: %v", env)
	}
}

// A control-holding client must be able to deregister another agent
// (it used to send its msg-layer token and get 403).
func TestDeregisterOtherWithControl(t *testing.T) {
	srv, nc := newTestNet(t)
	aliceTok := registerHTTP(t, srv, nc, "alice")
	registerHTTP(t, srv, nc, "bob")

	alice := agentClient(t, srv, aliceTok, nc.ControlToken)
	if err := alice.Deregister("bob"); err != nil {
		t.Fatalf("control-holding deregister of another agent: %v", err)
	}
	// And without control it is refused server-side.
	carolTok := registerHTTP(t, srv, nc, "carol")
	code, out := call(t, srv, "POST", "/v1/nets/dev/deregister", carolTok,
		map[string]any{"name": "alice"})
	if code != 403 {
		t.Fatalf("msg-layer deregister of another agent: want 403, got %d %v", code, out)
	}
}
