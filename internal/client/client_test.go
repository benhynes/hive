package client

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// A daemon bound to a specific address doesn't listen on loopback, so
// the default HIVE_ADDR must follow the configured bind.
func TestResolveAddrFollowsBind(t *testing.T) {
	cases := []struct {
		bind, want string
	}{
		{"192.168.1.9", "http://192.168.1.9:7345"},
		{"0.0.0.0", "http://127.0.0.1:7345"},
		{"::", "http://127.0.0.1:7345"},
		{"fd07::fe", "http://[fd07::fe]:7345"},
	}
	for _, c := range cases {
		home := t.TempDir()
		t.Setenv("HIVE_HOME", home)
		t.Setenv("HIVE_ADDR", "")
		t.Setenv("HIVE_NET", "dev")
		t.Setenv("HIVE_TOKEN", "x")
		cfg := fmt.Sprintf(`{"host_name":"h","bind":%q,"port":7345}`, c.bind)
		if err := os.WriteFile(home+"/config.json", []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		cl, err := Resolve("")
		if err != nil {
			t.Fatalf("bind %q: %v", c.bind, err)
		}
		if cl.Addr != c.want {
			t.Errorf("bind %q: addr %q, want %q", c.bind, cl.Addr, c.want)
		}
	}
}

func TestResolveHostLocalControl(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)
	t.Setenv("HIVE_ADDR", "")
	t.Setenv("HIVE_NET", "dev")
	t.Setenv("HIVE_TOKEN", "")
	t.Setenv("HIVE_CONTROL_TOKEN", "")
	t.Setenv("HIVE_CONTROL_HOST", "")
	if err := os.WriteFile(home+"/config.json", []byte(`{"host_name":"vm1","bind":"127.0.0.1","port":7777}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home+"/nets/dev", 0o700); err != nil {
		t.Fatal(err)
	}
	local := strings.Repeat("a", 64)
	msg := strings.Repeat("b", 64)
	netJSON := fmt.Sprintf(`{"name":"dev","msg_token":%q,"control_token":%q,"control_host":"vm1","hosts":{"vm1":"127.0.0.1:7777","mac":"127.0.0.1:9999"}}`, msg, local)
	if err := os.WriteFile(home+"/nets/dev/net.json", []byte(netJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != msg || c.Control != local || c.ControlHost != "vm1" {
		t.Fatalf("resolved token=%q control=%q host=%q", c.Token, c.Control, c.ControlHost)
	}
	if _, err := c.controlToken("vm1"); err != nil {
		t.Fatalf("local control rejected: %v", err)
	}
	if _, err := c.controlToken("mac"); err == nil || !strings.Contains(err.Error(), "scoped") {
		t.Fatalf("remote control not rejected locally: %v", err)
	}
}

func TestResolveSharedControlUsesMessageToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)
	t.Setenv("HIVE_ADDR", "")
	t.Setenv("HIVE_NET", "dev")
	t.Setenv("HIVE_TOKEN", "")
	t.Setenv("HIVE_CONTROL_TOKEN", "")
	t.Setenv("HIVE_CONTROL_HOST", "")
	if err := os.WriteFile(home+"/config.json", []byte(`{"host_name":"mac","bind":"127.0.0.1","port":7777}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home+"/nets/dev", 0o700); err != nil {
		t.Fatal(err)
	}
	shared := strings.Repeat("c", 64)
	msg := strings.Repeat("d", 64)
	netJSON := fmt.Sprintf(`{"name":"dev","msg_token":%q,"control_token":%q,"hosts":{"mac":"127.0.0.1:7777"}}`, msg, shared)
	if err := os.WriteFile(home+"/nets/dev/net.json", []byte(netJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != msg || c.Control != shared || c.ControlHost != "" {
		t.Fatalf("resolved token=%q control=%q host=%q", c.Token, c.Control, c.ControlHost)
	}
}

func TestResolveAgentTokenDoesNotInheritControl(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)
	t.Setenv("HIVE_ADDR", "")
	t.Setenv("HIVE_NET", "dev")
	personal := strings.Repeat("e", 64)
	t.Setenv("HIVE_TOKEN", personal)
	t.Setenv("HIVE_AGENT", "worker@vm1")
	t.Setenv("HIVE_CONTROL_TOKEN", "")
	t.Setenv("HIVE_CONTROL_HOST", "")
	if err := os.WriteFile(home+"/config.json", []byte(`{"host_name":"vm1","bind":"127.0.0.1","port":7777}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home+"/nets/dev", 0o700); err != nil {
		t.Fatal(err)
	}
	local := strings.Repeat("a", 64)
	msg := strings.Repeat("b", 64)
	netJSON := fmt.Sprintf(`{"name":"dev","msg_token":%q,"control_token":%q,"control_host":"vm1","hosts":{"vm1":"127.0.0.1:7777"}}`, msg, local)
	if err := os.WriteFile(home+"/nets/dev/net.json", []byte(netJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != personal {
		t.Fatalf("resolved token=%q, want personal token", c.Token)
	}
	if c.HasControl() || c.ControlHost != "" {
		t.Fatalf("agent inherited control=%q host=%q", c.Control, c.ControlHost)
	}
	if _, err := c.controlToken("vm1"); err == nil {
		t.Fatal("agent-scoped client unexpectedly obtained control")
	}
}
