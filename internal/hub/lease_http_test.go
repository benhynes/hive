package hub_test

import (
	"testing"
	"time"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/store"
)

func TestLeasedRegistrationHeartbeatAndExpiry(t *testing.T) {
	srv, nc := newTestNet(t)
	bootstrap := agentClient(t, srv, nc.MsgToken, "")
	registered, err := bootstrap.RegisterLease("ephemeral", "", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if registered.LeaseSeconds != 1 || registered.LeaseExpires <= time.Now().UnixMilli() {
		t.Fatalf("registration omitted lease metadata: %+v", registered)
	}

	// Network credentials may create registrations but cannot impersonate an
	// agent's presence after the per-agent credential is minted.
	if code, out := call(t, srv, "POST", "/v1/nets/dev/heartbeat", nc.MsgToken, nil); code != 403 {
		t.Fatalf("network-token heartbeat: want 403, got %d %v", code, out)
	}

	agent := agentClient(t, srv, registered.Token, "")
	time.Sleep(10 * time.Millisecond)
	beat, err := agent.Heartbeat()
	if err != nil {
		t.Fatal(err)
	}
	if beat.Agent != "ephemeral@testhost" || beat.LeaseSeconds != 1 || beat.LeaseExpires <= registered.LeaseExpires {
		t.Fatalf("heartbeat did not extend lease: registered=%+v heartbeat=%+v", registered, beat)
	}

	untilExpiry := time.Until(time.UnixMilli(beat.LeaseExpires))
	if untilExpiry > 0 {
		time.Sleep(untilExpiry + 20*time.Millisecond)
	}
	list, err := agent.Agents(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Agents) != 1 || list.Agents[0].Alive {
		t.Fatalf("expired lease still alive: %+v", list.Agents)
	}

	// Expiry changes presence, not authentication. If the name has not been
	// reclaimed, the original process can resume and renew its durable record.
	beat, err = agent.Heartbeat()
	if err != nil {
		t.Fatal(err)
	}
	if beat.LeaseExpires <= time.Now().UnixMilli() {
		t.Fatalf("resume heartbeat returned expired lease: %+v", beat)
	}
	list, err = agent.Agents(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Agents) != 1 || !list.Agents[0].Alive {
		t.Fatalf("renewed lease did not return online: %+v", list.Agents)
	}
}

func TestLegacyRegistrationAndHeartbeatRemainUnleased(t *testing.T) {
	srv, nc := newTestNet(t)
	bootstrap := agentClient(t, srv, nc.MsgToken, "")
	registered, err := bootstrap.Register("legacy", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if registered.LeaseSeconds != 0 || registered.LeaseExpires != 0 {
		t.Fatalf("legacy Register unexpectedly leased: %+v", registered)
	}
	agent := agentClient(t, srv, registered.Token, "")
	beat, err := agent.Heartbeat()
	if err != nil {
		t.Fatal(err)
	}
	if beat.LeaseSeconds != 0 || beat.LeaseExpires != 0 {
		t.Fatalf("legacy heartbeat should be a no-op: %+v", beat)
	}
}

func TestEphemeralLeaseFlagStoredOnlyWhenRequested(t *testing.T) {
	srv, nc := newTestNet(t)
	bootstrap := agentClient(t, srv, nc.MsgToken, "")
	generatedResp, err := bootstrap.RegisterEphemeralLease("generated", 45)
	if err != nil {
		t.Fatal(err)
	}
	if !generatedResp.Ephemeral {
		t.Fatalf("ephemeral registration response omitted acknowledgement: %+v", generatedResp)
	}
	namedResp, err := bootstrap.RegisterLease("named", "", 0, 45)
	if err != nil {
		t.Fatal(err)
	}
	if namedResp.Ephemeral {
		t.Fatalf("named registration response unexpectedly ephemeral: %+v", namedResp)
	}

	reg, err := store.OpenRegistry(config.NetDir("dev"))
	if err != nil {
		t.Fatal(err)
	}
	generated, ok := reg.Get("generated")
	if !ok || !generated.Ephemeral {
		t.Fatalf("generated registration missing ephemeral flag: ok=%v rec=%+v", ok, generated)
	}
	named, ok := reg.Get("named")
	if !ok || named.Ephemeral {
		t.Fatalf("explicitly named registration became ephemeral: ok=%v rec=%+v", ok, named)
	}
}

func TestNamedReleaseStaysAddressableAndReclaimsMailbox(t *testing.T) {
	srv, nc := newTestNet(t)
	bootstrap := agentClient(t, srv, nc.MsgToken, "")
	registered, err := bootstrap.RegisterLease("worker", "", 0, 60)
	if err != nil {
		t.Fatal(err)
	}
	old := agentClient(t, srv, registered.Token, "")
	if _, err := old.ReleaseLease(); err != nil {
		t.Fatal(err)
	}
	list, err := bootstrap.Agents(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Agents) != 1 || list.Agents[0].Alive || list.Agents[0].Ephemeral {
		t.Fatalf("released named identity = %+v, want retained offline", list.Agents)
	}
	sent, err := bootstrap.Send("worker@testhost", "msg", "queued offline", "")
	if err != nil || sent.Results["worker@testhost"] != "delivered" {
		t.Fatalf("offline send: result=%+v err=%v", sent, err)
	}
	replacement, err := bootstrap.RegisterLease("worker", "", 0, 60)
	if err != nil {
		t.Fatal(err)
	}
	next := agentClient(t, srv, replacement.Token, "")
	inbox, err := next.Inbox(0, 0, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox.Msgs) != 1 || inbox.Msgs[0].Env.Body != "queued offline" {
		t.Fatalf("replacement did not inherit retained mail: %+v", inbox)
	}
}
