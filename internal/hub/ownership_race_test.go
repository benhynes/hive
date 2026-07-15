package hub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/proto"
	"github.com/benhynes/hive/internal/store"
)

type ownershipHTTPResult struct {
	code int
	body string
}

type gatedRequestBody struct {
	reader  *strings.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *gatedRequestBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	return b.reader.Read(p)
}

func (b *gatedRequestBody) Close() error { return nil }

func newOwnershipHub(t *testing.T) (*Hub, *network, *httptest.Server, config.NetConfig) {
	t.Helper()
	t.Setenv("HIVE_HOME", t.TempDir())
	nc := config.NetConfig{
		Name: "dev", MsgToken: proto.NewToken(), ControlToken: proto.NewToken(),
		Hosts: map[string]string{"testhost": "127.0.0.1:1"},
	}
	if err := config.SaveNet(nc); err != nil {
		t.Fatal(err)
	}
	h := New(config.Config{HostName: "testhost", Bind: "127.0.0.1", Port: 1})
	n, err := h.net("dev")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		srv.Close()
		h.Shutdown()
	})
	return h, n, srv, nc
}

func ownershipCall(t *testing.T, srv *httptest.Server, method, path, token string, body any) ownershipHTTPResult {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return ownershipHTTPResult{code: resp.StatusCode, body: string(b)}
}

func ownershipRegister(t *testing.T, srv *httptest.Server, token, name string) string {
	t.Helper()
	res := ownershipCall(t, srv, http.MethodPost, "/v1/nets/dev/register", token, map[string]any{"name": name})
	if res.code != http.StatusOK {
		t.Fatalf("register %s: status=%d body=%s", name, res.code, res.body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(res.body), &out); err != nil || out.Token == "" {
		t.Fatalf("register %s response=%q err=%v", name, res.body, err)
	}
	return out.Token
}

func TestLongPollCannotReturnMailAfterNameReclaimed(t *testing.T) {
	_, n, srv, nc := newOwnershipHub(t)
	oldToken := ownershipRegister(t, srv, nc.MsgToken, "alice")
	ib, err := n.inbox("alice")
	if err != nil {
		t.Fatal(err)
	}

	pollDone := make(chan ownershipHTTPResult, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/nets/dev/inbox?after=0&wait=25", nil)
		if err != nil {
			pollDone <- ownershipHTTPResult{body: err.Error()}
			return
		}
		req.Header.Set("Authorization", "Bearer "+oldToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			pollDone <- ownershipHTTPResult{body: err.Error()}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		pollDone <- ownershipHTTPResult{code: resp.StatusCode, body: string(b)}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for ib.Pollers() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := ib.Pollers(); got != 1 {
		t.Fatalf("long poll never blocked; pollers=%d", got)
	}

	if res := ownershipCall(t, srv, http.MethodPost, "/v1/nets/dev/deregister", oldToken, map[string]any{"name": ""}); res.code != http.StatusOK {
		t.Fatalf("deregister old generation: status=%d body=%s", res.code, res.body)
	}
	newToken := ownershipRegister(t, srv, nc.MsgToken, "alice")
	const secret = "mail for the replacement only"
	if res := ownershipCall(t, srv, http.MethodPost, "/v1/nets/dev/send", nc.MsgToken,
		map[string]any{"to": "alice@testhost", "body": secret}); res.code != http.StatusOK {
		t.Fatalf("send replacement mail: status=%d body=%s", res.code, res.body)
	}

	select {
	case res := <-pollDone:
		if res.code != http.StatusConflict {
			t.Fatalf("stale long poll status=%d body=%s, want 409", res.code, res.body)
		}
		if strings.Contains(res.body, secret) {
			t.Fatalf("stale long poll leaked replacement mail: %s", res.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale long poll did not return after replacement mail arrived")
	}

	res := ownershipCall(t, srv, http.MethodGet, "/v1/nets/dev/inbox?after=0", newToken, nil)
	if res.code != http.StatusOK || !strings.Contains(res.body, secret) {
		t.Fatalf("replacement could not read its mail: status=%d body=%s", res.code, res.body)
	}
}

func TestStaleAckCannotAdvanceReplacementCursor(t *testing.T) {
	h, n, srv, nc := newOwnershipHub(t)
	oldToken := ownershipRegister(t, srv, nc.MsgToken, "alice")
	body := &gatedRequestBody{
		reader: strings.NewReader(`{"seq":1}`), started: make(chan struct{}), release: make(chan struct{}),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/nets/dev/ack", body)
	req.Header.Set("Authorization", "Bearer "+oldToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.Handler().ServeHTTP(rr, req)
		close(done)
	}()

	select {
	case <-body.started:
	case <-time.After(2 * time.Second):
		t.Fatal("ack handler did not begin reading its body")
	}
	if res := ownershipCall(t, srv, http.MethodPost, "/v1/nets/dev/deregister", oldToken, map[string]any{"name": ""}); res.code != http.StatusOK {
		t.Fatalf("deregister old generation: status=%d body=%s", res.code, res.body)
	}
	newToken := ownershipRegister(t, srv, nc.MsgToken, "alice")
	const secret = "replacement cursor must not skip this"
	if res := ownershipCall(t, srv, http.MethodPost, "/v1/nets/dev/send", nc.MsgToken,
		map[string]any{"to": "alice@testhost", "body": secret}); res.code != http.StatusOK {
		t.Fatalf("send replacement mail: status=%d body=%s", res.code, res.body)
	}
	close(body.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stale ack did not finish")
	}
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale ack status=%d body=%s, want 409", rr.Code, rr.Body.String())
	}
	ib, err := n.inbox("alice")
	if err != nil {
		t.Fatal(err)
	}
	if cursor := ib.Cursor(); cursor != 0 {
		t.Fatalf("stale ack advanced replacement cursor to %d", cursor)
	}
	res := ownershipCall(t, srv, http.MethodGet, "/v1/nets/dev/inbox", newToken, nil)
	if res.code != http.StatusOK || !strings.Contains(res.body, secret) {
		t.Fatalf("replacement mail was skipped: status=%d body=%s", res.code, res.body)
	}
}

func TestStaleSendCannotImpersonateReplacement(t *testing.T) {
	h, _, srv, nc := newOwnershipHub(t)
	oldToken := ownershipRegister(t, srv, nc.MsgToken, "alice")
	bobToken := ownershipRegister(t, srv, nc.MsgToken, "bob")
	body := &gatedRequestBody{
		reader:  strings.NewReader(`{"to":"bob@testhost","body":"from stale generation"}`),
		started: make(chan struct{}), release: make(chan struct{}),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/nets/dev/send", body)
	req.SetPathValue("net", "dev")
	req.Header.Set("Authorization", "Bearer "+oldToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.Handler().ServeHTTP(rr, req)
		close(done)
	}()

	select {
	case <-body.started:
	case <-time.After(2 * time.Second):
		t.Fatal("send handler did not begin reading its body")
	}
	if res := ownershipCall(t, srv, http.MethodPost, "/v1/nets/dev/deregister", oldToken, map[string]any{"name": ""}); res.code != http.StatusOK {
		t.Fatalf("deregister old generation: status=%d body=%s", res.code, res.body)
	}
	_ = ownershipRegister(t, srv, nc.MsgToken, "alice")
	close(body.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stale send did not finish")
	}
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale send status=%d body=%s, want 409", rr.Code, rr.Body.String())
	}
	res := ownershipCall(t, srv, http.MethodGet, "/v1/nets/dev/inbox?after=0", bobToken, nil)
	if res.code != http.StatusOK || strings.Contains(res.body, "from stale generation") {
		t.Fatalf("stale send reached replacement inbox: status=%d body=%s", res.code, res.body)
	}
}

func TestStaleKillCannotDeleteNewClaimant(t *testing.T) {
	h, n, srv, nc := newOwnershipHub(t)
	oldToken := proto.NewToken()
	if err := n.reg.Put(store.AgentRec{
		Name: "worker", TokenHash: proto.HashToken(oldToken), Spawned: true,
		Session: "old-session", Pane: "%old", Registered: time.Now().UnixMilli(),
		LeaseSeconds: 1, LeaseExpires: time.Now().Add(-time.Second).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	teardownStarted := make(chan struct{})
	releaseTeardown := make(chan struct{})
	h.killSessionFn = func(session, pane string) error {
		if session != "old-session" || pane != "%old" {
			t.Errorf("teardown target=(%q,%q), want old registration", session, pane)
		}
		close(teardownStarted)
		<-releaseTeardown
		return nil
	}

	killDone := make(chan ownershipHTTPResult, 1)
	go func() {
		killDone <- ownershipCall(t, srv, http.MethodPost, "/v1/nets/dev/kill", nc.ControlToken,
			map[string]any{"agent": "worker@testhost"})
	}()
	select {
	case <-teardownStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("kill did not reach the teardown seam")
	}

	newToken := ownershipRegister(t, srv, nc.MsgToken, "worker")
	close(releaseTeardown)
	select {
	case res := <-killDone:
		if res.code != http.StatusOK {
			t.Fatalf("kill status=%d body=%s", res.code, res.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("kill did not finish after teardown was released")
	}

	rec, ok := n.reg.Get("worker")
	if !ok || rec.TokenHash != proto.HashToken(newToken) {
		t.Fatalf("stale kill deleted replacement: ok=%v rec=%+v", ok, rec)
	}
	if res := ownershipCall(t, srv, http.MethodGet, "/v1/nets/dev/inbox?after=0", newToken, nil); res.code != http.StatusOK {
		t.Fatalf("replacement token no longer resolves: status=%d body=%s", res.code, res.body)
	}
}
