package hub

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/proto"
	"github.com/benhynes/hive/internal/store"
)

func TestAliveHonorsPresenceLease(t *testing.T) {
	now := time.Now()
	if alive(store.AgentRec{LeaseSeconds: 45, LeaseExpires: now.Add(-time.Millisecond).UnixMilli()}) {
		t.Fatal("expired unbound lease reported alive")
	}
	if alive(store.AgentRec{LeaseSeconds: 45}) {
		t.Fatal("leased record with no expiry reported alive")
	}
	if !alive(store.AgentRec{LeaseSeconds: 45, LeaseExpires: now.Add(time.Minute).UnixMilli()}) {
		t.Fatal("current unbound lease reported dead")
	}
	if !alive(store.AgentRec{}) {
		t.Fatal("legacy unbound registration lost backward-compatible liveness")
	}
}

// Simulate an old deregistration request that authenticated immediately
// before an expired name was reclaimed. The handler must re-check the token
// under regMu instead of deleting the replacement by name alone.
func TestStaleDeregisterCannotDeleteReplacement(t *testing.T) {
	reg, err := store.OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldToken := proto.NewToken()
	newToken := proto.NewToken()
	if err := reg.Put(store.AgentRec{
		Name: "alice", TokenHash: proto.HashToken(newToken), Registered: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	n := &network{name: "dev", reg: reg}
	h := &Hub{Cfg: config.Config{HostName: "host"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/nets/dev/deregister", strings.NewReader(`{"name":""}`))
	req.Header.Set("Authorization", "Bearer "+oldToken)
	w := httptest.NewRecorder()

	h.hDeregister(w, req, n, ident{Agent: "alice", TokenHash: proto.HashToken(oldToken)})
	if w.Code != http.StatusConflict {
		t.Fatalf("stale deregister status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	got, ok := reg.Get("alice")
	if !ok || got.TokenHash != proto.HashToken(newToken) {
		t.Fatalf("replacement registration was deleted: ok=%v rec=%+v", ok, got)
	}
}

func TestPruneRetiresEphemeralMailboxBeforeNameReuse(t *testing.T) {
	dir := t.TempDir()
	reg, err := store.OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rec := store.AgentRec{
		Name: "generated", TokenHash: proto.HashToken(proto.NewToken()), Ephemeral: true,
		LeaseSeconds: 45, LeaseExpires: now.Add(-ephemeralRetention - time.Second).UnixMilli(),
	}
	if err := reg.Put(rec); err != nil {
		t.Fatal(err)
	}
	n := &network{
		name: "dev", dir: dir, reg: reg,
		inboxes: map[string]*store.Inbox{}, lastNudge: map[string]time.Time{}, lastNudgedLatest: map[string]int64{},
	}
	ib, err := n.inbox(rec.Name)
	if err != nil {
		t.Fatal(err)
	}
	msg := proto.Envelope{ID: "mail", From: "a@host", To: "generated@host", Kind: proto.KindMsg, Body: "secret", TS: 1}
	seq, _, err := ib.Append(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := ib.Ack(seq); err != nil {
		t.Fatal(err)
	}
	n.lastNudge[rec.Name] = now
	n.lastNudgedLatest[rec.Name] = seq

	pruned, err := pruneExpiredEphemeral(n, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != rec.Name {
		t.Fatalf("pruned = %v", pruned)
	}
	if _, ok := reg.Get(rec.Name); ok {
		t.Fatal("ephemeral registry record survived prune")
	}
	n.mu.Lock()
	_, cached := n.inboxes[rec.Name]
	_, nudged := n.lastNudge[rec.Name]
	_, latest := n.lastNudgedLatest[rec.Name]
	n.mu.Unlock()
	if cached || nudged || latest {
		t.Fatalf("ephemeral in-memory state survived: inbox=%v nudge=%v latest=%v", cached, nudged, latest)
	}
	for _, path := range []string{
		filepath.Join(dir, "inbox", rec.Name+".jsonl"),
		filepath.Join(dir, "cursors", rec.Name),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("ephemeral mailbox file %s survived: %v", path, err)
		}
	}
	if _, _, err := ib.Append(msg); !errors.Is(err, store.ErrInboxRetired) {
		t.Fatalf("stale inbox append = %v, want retired", err)
	}
	fresh, err := n.inbox(rec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got := fresh.Read(0, 10); len(got.Msgs) != 0 || got.Cursor != 0 || got.Latest != 0 {
		t.Fatalf("replacement name inherited ephemeral mail: %+v", got)
	}
}

func TestBroadcastSkipsExpiredHiddenEphemeralIdentity(t *testing.T) {
	dir := t.TempDir()
	reg, err := store.OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, rec := range []store.AgentRec{
		{Name: "generated", TokenHash: "generated-token", Ephemeral: true, LeaseSeconds: 60, LeaseExpires: now.Add(-time.Second).UnixMilli()},
		{Name: "retained", TokenHash: "retained-token", LeaseSeconds: 60, LeaseExpires: now.Add(-time.Second).UnixMilli()},
	} {
		if err := reg.Put(rec); err != nil {
			t.Fatal(err)
		}
	}
	n := &network{
		name: "dev", dir: dir, reg: reg,
		inboxes: map[string]*store.Inbox{}, lastNudge: map[string]time.Time{}, lastNudgedLatest: map[string]int64{},
	}
	h := &Hub{Cfg: config.Config{HostName: "host"}}
	results := h.broadcastLocal(n, proto.Envelope{
		ID: "mail", From: "sender@host", To: proto.Broadcast,
		Kind: proto.KindMsg, Body: "work", TS: now.UnixMilli(),
	}, "")
	if _, ok := results["generated"]; ok {
		t.Fatalf("hidden disposable identity received broadcast: %v", results)
	}
	if results["retained"] != "delivered" {
		t.Fatalf("retained offline identity missed broadcast: %v", results)
	}
}

func TestExpiredEphemeralHiddenDuringGraceAndRecoverable(t *testing.T) {
	t.Setenv("HIVE_HOME", t.TempDir())
	if err := config.SaveNet(config.NetConfig{
		Name: "dev", MsgToken: proto.NewToken(), ControlToken: proto.NewToken(),
		Hosts: map[string]string{"testhost": "127.0.0.1:1"},
	}); err != nil {
		t.Fatal(err)
	}
	h := New(config.Config{HostName: "testhost"})
	n, err := h.net("dev")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rec := store.AgentRec{
		Name: "generated", TokenHash: "generated-token", Ephemeral: true,
		LeaseSeconds: 60, LastSeen: now.Add(-time.Hour - time.Minute).UnixMilli(),
		LeaseExpires: now.Add(-time.Hour).UnixMilli(),
	}
	if err := n.reg.Put(rec); err != nil {
		t.Fatal(err)
	}
	ib, err := n.inbox(rec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ib.Append(proto.Envelope{
		ID: "waiting", From: "sender@testhost", To: "generated@testhost",
		Kind: proto.KindMsg, Body: "recover me", TS: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	if got := h.localAgents(n); len(got) != 0 {
		t.Fatalf("expired ephemeral identity remained discoverable during grace: %+v", got)
	}
	if pruned, err := pruneExpiredEphemeral(n, now); err != nil || len(pruned) != 0 {
		t.Fatalf("within-grace prune = %v, err=%v", pruned, err)
	}
	if got, ok := n.reg.ByToken(rec.TokenHash); !ok || got.Name != rec.Name {
		t.Fatalf("within-grace token was retired: ok=%v rec=%+v", ok, got)
	}

	metrics := httptest.NewRecorder()
	h.hMetrics(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`hive_agents{host="testhost",network="dev",state="alive"} 0`,
		`hive_agents{host="testhost",network="dev",state="dead"} 0`,
		`hive_inbox_lag_messages{host="testhost",network="dev"} 0`,
	} {
		if !strings.Contains(metrics.Body.String(), want) {
			t.Errorf("metrics exposed expired ephemeral state; missing %q:\n%s", want, metrics.Body.String())
		}
	}

	if _, ok, err := n.reg.RenewLease(rec.Name, rec.TokenHash, time.Now()); err != nil || !ok {
		t.Fatalf("recovery heartbeat: ok=%v err=%v", ok, err)
	}
	if got := h.localAgents(n); len(got) != 1 || !got[0].Alive || !got[0].Ephemeral {
		t.Fatalf("renewed ephemeral identity did not reappear: %+v", got)
	}
	metrics = httptest.NewRecorder()
	h.hMetrics(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`hive_agents{host="testhost",network="dev",state="alive"} 1`,
		`hive_inbox_lag_messages{host="testhost",network="dev"} 1`,
	} {
		if !strings.Contains(metrics.Body.String(), want) {
			t.Errorf("metrics did not restore renewed ephemeral state; missing %q:\n%s", want, metrics.Body.String())
		}
	}
}
