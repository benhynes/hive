package hub

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/store"
)

func TestHostLocalControlAuthentication(t *testing.T) {
	reg, err := store.OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := &network{cfg: config.NetConfig{
		MsgToken: "msg", ControlToken: "local", ControlHost: "vm1",
	}, reg: reg}
	if id, ok := n.resolve("local", "vm1"); !ok || !id.Control {
		t.Fatal("host-local token rejected by its bound hub")
	}
	if _, ok := n.resolve("local", "mac"); ok {
		t.Fatal("host-local token accepted by a different hub")
	}
}

func TestRotateControlRevokesOldTokenAndPersistsLocalScope(t *testing.T) {
	t.Setenv("HIVE_HOME", t.TempDir())
	old := strings.Repeat("a", 64)
	fresh := strings.Repeat("b", 64)
	nc := config.NetConfig{
		Name: "dev", MsgToken: strings.Repeat("c", 64), ControlToken: old,
		Hosts: map[string]string{"mac": "127.0.0.1:7777"},
	}
	if err := config.SaveNet(nc); err != nil {
		t.Fatal(err)
	}
	reg, err := store.OpenRegistry(config.NetDir("dev"))
	if err != nil {
		t.Fatal(err)
	}
	audit, err := os.OpenFile(config.NetDir("dev")+"/audit.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer audit.Close()
	n := &network{name: "dev", cfg: nc, reg: reg, audit: audit}
	h := New(config.Config{HostName: "mac", Bind: "127.0.0.1", Port: 7777})
	req := httptest.NewRequest("POST", "/v1/nets/dev/control/rotate", strings.NewReader(`{"token":"`+fresh+`"}`))
	rr := httptest.NewRecorder()
	h.hRotateControl(rr, req, n, ident{Control: true, NetTok: true})
	if rr.Code != 200 {
		t.Fatalf("rotate status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, ok := n.resolve(old, "mac"); ok {
		t.Fatal("old control token remained valid")
	}
	if id, ok := n.resolve(fresh, "mac"); !ok || !id.Control {
		t.Fatal("new control token is not valid locally")
	}
	if _, ok := n.resolve(fresh, "peer"); ok {
		t.Fatal("rotated token is valid on a peer")
	}
	saved, err := config.LoadNet("dev")
	if err != nil {
		t.Fatal(err)
	}
	if saved.ControlToken != fresh || saved.ControlHost != "mac" {
		t.Fatalf("saved control=%q host=%q", saved.ControlToken, saved.ControlHost)
	}
}

func TestSpawnEnvPropagatesControlHost(t *testing.T) {
	h := New(config.Config{HostName: "vm1", Bind: "127.0.0.1", Port: 7777})
	n := &network{name: "dev", cfg: config.NetConfig{
		ControlToken: "local", ControlHost: "vm1",
	}}
	_, env, err := h.spawnEnv(n, spawnReq{Name: "worker", GrantControl: true})
	if err != nil {
		t.Fatal(err)
	}
	if env["HIVE_CONTROL_TOKEN"] != "local" || env["HIVE_CONTROL_HOST"] != "vm1" {
		t.Fatalf("spawn env did not retain local scope: %#v", env)
	}

	other := New(config.Config{HostName: "mac", Bind: "127.0.0.1", Port: 7777})
	if _, _, err := other.spawnEnv(n, spawnReq{Name: "worker", GrantControl: true}); err == nil {
		t.Fatal("copied host-local token was granted by a different hub")
	}
}
