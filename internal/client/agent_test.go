package client

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func writeAgentResolveConfig(t *testing.T, msg, control string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)
	t.Setenv("HIVE_ADDR", "")
	t.Setenv("HIVE_NET", "dev")
	t.Setenv("HIVE_TOKEN", "")
	t.Setenv("HIVE_AGENT", "")
	t.Setenv("HIVE_CONTROL_TOKEN", "")
	t.Setenv("HIVE_CONTROL_HOST", "")
	if err := os.WriteFile(home+"/config.json", []byte(`{"host_name":"vm1","bind":"127.0.0.1","port":7777}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home+"/nets/dev", 0o700); err != nil {
		t.Fatal(err)
	}
	netJSON := fmt.Sprintf(`{"name":"dev","msg_token":%q,"control_token":%q,"control_host":"vm1","hosts":{"vm1":"127.0.0.1:7777"}}`, msg, control)
	if err := os.WriteFile(home+"/nets/dev/net.json", []byte(netJSON), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveAgentUsesMessageLayerOnly(t *testing.T) {
	msg := strings.Repeat("a", 64)
	writeAgentResolveConfig(t, msg, strings.Repeat("b", 64))
	c, err := ResolveAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != msg {
		t.Fatalf("token=%q, want MSG token", c.Token)
	}
	if c.HasControl() || c.ControlHost != "" {
		t.Fatalf("agent inherited CONTROL=%q host=%q", c.Control, c.ControlHost)
	}
}

func TestResolveAgentKeepsExplicitControl(t *testing.T) {
	msg := strings.Repeat("a", 64)
	explicit := strings.Repeat("c", 64)
	writeAgentResolveConfig(t, msg, strings.Repeat("b", 64))
	t.Setenv("HIVE_CONTROL_TOKEN", explicit)
	t.Setenv("HIVE_CONTROL_HOST", "vm1")
	c, err := ResolveAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != msg || c.Control != explicit || c.ControlHost != "vm1" {
		t.Fatalf("resolved token=%q control=%q host=%q", c.Token, c.Control, c.ControlHost)
	}
}

func TestResolveAgentRefusesControlFallback(t *testing.T) {
	writeAgentResolveConfig(t, "", strings.Repeat("b", 64))
	_, err := ResolveAgent("")
	if err == nil || !strings.Contains(err.Error(), "refusing to use CONTROL") {
		t.Fatalf("expected fail-closed missing-MSG error, got %v", err)
	}
}

func TestResolveAgentAcceptsExplicitPersonalToken(t *testing.T) {
	writeAgentResolveConfig(t, "", strings.Repeat("b", 64))
	personal := strings.Repeat("d", 64)
	t.Setenv("HIVE_TOKEN", personal)
	t.Setenv("HIVE_AGENT", "worker@vm1")
	c, err := ResolveAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != personal || c.HasControl() {
		t.Fatalf("resolved token=%q control=%q", c.Token, c.Control)
	}
}

func TestResolveAgentRejectsHalfIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, token, agent string
	}{
		{name: "token only", token: strings.Repeat("d", 64)},
		{name: "agent only", agent: "worker@vm1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeAgentResolveConfig(t, strings.Repeat("a", 64), strings.Repeat("b", 64))
			t.Setenv("HIVE_TOKEN", tc.token)
			t.Setenv("HIVE_AGENT", tc.agent)
			_, err := ResolveAgent("")
			if err == nil || !strings.Contains(err.Error(), "must be set together") {
				t.Fatalf("expected paired-identity error, got %v", err)
			}
		})
	}
}

func TestResolveBootstrapReplacesEnclosingIdentity(t *testing.T) {
	msg := strings.Repeat("a", 64)
	writeAgentResolveConfig(t, msg, strings.Repeat("b", 64))
	t.Setenv("HIVE_AGENT", "parent@vm1")
	t.Setenv("HIVE_TOKEN", strings.Repeat("c", 64))

	c, err := ResolveBootstrap("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != msg {
		t.Fatalf("bootstrap token=%q, want configured MSG token", c.Token)
	}
	if c.Agent != "" || c.HasControl() || c.ControlHost != "" {
		t.Fatalf("bootstrap retained parent capability: agent=%q control=%q host=%q", c.Agent, c.Control, c.ControlHost)
	}
}

func TestResolveBootstrapPreservesExplicitNetworkToken(t *testing.T) {
	writeAgentResolveConfig(t, strings.Repeat("a", 64), strings.Repeat("b", 64))
	explicit := strings.Repeat("e", 64)
	t.Setenv("HIVE_TOKEN", explicit)
	t.Setenv("HIVE_AGENT", "")

	c, err := ResolveBootstrap("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != explicit {
		t.Fatalf("bootstrap token=%q, want explicit environment credential", c.Token)
	}
}

func TestResolveBootstrapRefusesImplicitControlFallback(t *testing.T) {
	writeAgentResolveConfig(t, "", strings.Repeat("b", 64))

	_, err := ResolveBootstrap("")
	if err == nil || !strings.Contains(err.Error(), "refusing to use CONTROL") {
		t.Fatalf("expected fail-closed bootstrap error, got %v", err)
	}
}

func TestResolveBootstrapCanUseExplicitControl(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)
	t.Setenv("HIVE_ADDR", "")
	t.Setenv("HIVE_NET", "dev")
	t.Setenv("HIVE_AGENT", "parent@vm1")
	t.Setenv("HIVE_TOKEN", strings.Repeat("c", 64))
	control := strings.Repeat("d", 64)
	t.Setenv("HIVE_CONTROL_TOKEN", control)
	t.Setenv("HIVE_CONTROL_HOST", "vm1")
	if err := os.WriteFile(home+"/config.json", []byte(`{"host_name":"vm1","bind":"127.0.0.1","port":7777}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := ResolveBootstrap("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != control {
		t.Fatalf("bootstrap token=%q, want explicit CONTROL used once", c.Token)
	}
	if c.Agent != "" || c.HasControl() || c.ControlHost != "" {
		t.Fatalf("bootstrap retained parent capability: agent=%q control=%q host=%q", c.Agent, c.Control, c.ControlHost)
	}
}

func TestResolveBootstrapRejectsControlForDifferentHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)
	t.Setenv("HIVE_ADDR", "")
	t.Setenv("HIVE_NET", "dev")
	t.Setenv("HIVE_AGENT", "parent@vm1")
	t.Setenv("HIVE_TOKEN", strings.Repeat("c", 64))
	t.Setenv("HIVE_CONTROL_TOKEN", strings.Repeat("d", 64))
	t.Setenv("HIVE_CONTROL_HOST", "other-host")
	if err := os.WriteFile(home+"/config.json", []byte(`{"host_name":"vm1","bind":"127.0.0.1","port":7777}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveBootstrap("")
	if err == nil || !strings.Contains(err.Error(), "scoped to host") {
		t.Fatalf("expected host-scoped CONTROL rejection, got %v", err)
	}
}
