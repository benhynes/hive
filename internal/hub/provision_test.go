package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhynes/hive/internal/config"
)

// readJSONFile decodes a JSON file into a generic map for assertions.
func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func TestProvisionCodexRuntimeIntoWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")
	auth := filepath.Join(home, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"auth_mode":"chatgpt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	prof := config.SpawnProfile{
		RuntimeSetup: &config.RuntimeSetup{Type: "codex", AuthSource: auth},
		Sandbox:      &config.SandboxRunner{Command: "/usr/local/bin/ff", Profiles: "/etc/runner.yaml", Profile: "codex"},
	}
	if err := provisionAgent(cwd, prof, "/host/hive"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(cwd, ".codex", "auth.json")); err != nil || string(got) != `{"auth_mode":"chatgpt"}` {
		t.Fatalf("codex auth = %q, %v", got, err)
	}
	configText, err := os.ReadFile(filepath.Join(cwd, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configText), `[mcp_servers.hive]`) ||
		!strings.Contains(string(configText), `command = "/usr/local/bin/hive"`) ||
		!strings.Contains(string(configText), `"/workspace/.hive-mcp.log"`) ||
		!strings.Contains(string(configText), `"HIVE_AGENT"`) ||
		!strings.Contains(string(configText), `[projects."/workspace"]`) {
		t.Fatalf("codex config = %s", configText)
	}
}

func TestProvisionClaudeRuntimePreapprovesSandboxWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")
	auth := filepath.Join(home, "credentials.json")
	state := filepath.Join(home, "claude.json")
	if err := os.WriteFile(auth, []byte(`{"oauth":"present"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte(`{"numStartups":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	prof := config.SpawnProfile{
		RuntimeSetup: &config.RuntimeSetup{
			Type: "claude", AuthSource: auth, StateSource: state, Workspace: "/workspace",
		},
		Sandbox: &config.SandboxRunner{Command: "/usr/local/bin/ff", Profiles: "/etc/runner.yaml", Profile: "claude"},
	}
	if err := provisionAgent(cwd, prof, "/host/hive"); err != nil {
		t.Fatal(err)
	}
	got := readJSONFile(t, filepath.Join(cwd, ".claude", ".claude.json"))
	projects := got["projects"].(map[string]any)
	entry := projects["/workspace"].(map[string]any)
	if entry["hasTrustDialogAccepted"] != true || !contains(entry["enabledMcpjsonServers"].([]any), "hive") {
		t.Fatalf("Claude sandbox approval = %#v", entry)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".claude", ".credentials.json")); err != nil {
		t.Fatal(err)
	}
}

func TestProvisionAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // os.UserHomeDir() resolves to this; real config untouched
	cwd := filepath.Join(home, "work")

	// A context file to seed.
	ctxSrc := filepath.Join(t.TempDir(), "AGENT-GUIDE.md")
	if err := os.WriteFile(ctxSrc, []byte("mesh conventions"), 0o644); err != nil {
		t.Fatal(err)
	}

	prof := config.SpawnProfile{
		Context: []string{ctxSrc},
		MCP:     map[string]config.MCPServer{"playwright": {Command: "npx", Args: []string{"-y", "@playwright/mcp"}}},
	}
	if err := provisionAgent(cwd, prof, "/opt/hive"); err != nil {
		t.Fatalf("provisionAgent: %v", err)
	}

	// (1) .mcp.json lists hive (auto) + the profile server, in the right shape.
	mcp := readJSONFile(t, filepath.Join(cwd, ".mcp.json"))
	servers, ok := mcp["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf(".mcp.json missing mcpServers: %v", mcp)
	}
	hive, ok := servers["hive"].(map[string]any)
	if !ok {
		t.Fatalf("hive server not registered: %v", servers)
	}
	if hive["command"] != "/opt/hive" {
		t.Errorf("hive command = %v, want the passed hive binary path", hive["command"])
	}
	if _, ok := servers["playwright"]; !ok {
		t.Errorf("profile MCP server not registered: %v", servers)
	}

	// (2) context file seeded under its basename.
	if b, err := os.ReadFile(filepath.Join(cwd, "AGENT-GUIDE.md")); err != nil || string(b) != "mesh conventions" {
		t.Errorf("context file not seeded: %v / %q", err, b)
	}

	// (3) BOTH Claude Code gates pre-approved for cwd in ~/.claude.json.
	claude := readJSONFile(t, filepath.Join(home, ".claude.json"))
	projects := claude["projects"].(map[string]any)
	abs, _ := filepath.Abs(cwd)
	entry, ok := projects[abs].(map[string]any)
	if !ok {
		t.Fatalf("no project entry for %s in ~/.claude.json: %v", abs, projects)
	}
	if entry["hasTrustDialogAccepted"] != true {
		t.Errorf("workspace-trust gate not cleared: %v", entry["hasTrustDialogAccepted"])
	}
	enabled := entry["enabledMcpjsonServers"].([]any)
	if !contains(enabled, "hive") || !contains(enabled, "playwright") {
		t.Errorf("MCP-approval gate missing servers: %v", enabled)
	}
}

// The preseed must not clobber an existing ~/.claude.json: other projects and
// top-level fields survive, and an already-approved server list is unioned.
func TestPreseedPreservesExistingClaudeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")

	existing := map[string]any{
		"numStartups": 42.0,
		"projects": map[string]any{
			"/some/other/project": map[string]any{"hasTrustDialogAccepted": true},
			mustAbs(t, cwd):       map[string]any{"enabledMcpjsonServers": []any{"preexisting"}},
		},
	}
	b, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := provisionAgent(cwd, config.SpawnProfile{}, "hive"); err != nil {
		t.Fatalf("provisionAgent: %v", err)
	}

	claude := readJSONFile(t, filepath.Join(home, ".claude.json"))
	if claude["numStartups"] != 42.0 {
		t.Errorf("top-level field clobbered: numStartups=%v", claude["numStartups"])
	}
	projects := claude["projects"].(map[string]any)
	if _, ok := projects["/some/other/project"]; !ok {
		t.Errorf("unrelated project dropped: %v", projects)
	}
	entry := projects[mustAbs(t, cwd)].(map[string]any)
	enabled := entry["enabledMcpjsonServers"].([]any)
	if !contains(enabled, "preexisting") || !contains(enabled, "hive") {
		t.Errorf("enabledMcpjsonServers not unioned with existing: %v", enabled)
	}
}

func TestProvisionNoHiveMCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")

	// No profile servers and hive opted out → nothing to register, no files.
	if err := provisionAgent(cwd, config.SpawnProfile{NoHiveMCP: true}, "hive"); err != nil {
		t.Fatalf("provisionAgent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".mcp.json")); !os.IsNotExist(err) {
		t.Errorf(".mcp.json written when there were no servers to register")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf("~/.claude.json touched when there was nothing to approve")
	}
}

func contains(arr []any, s string) bool {
	for _, v := range arr {
		if v == s {
			return true
		}
	}
	return false
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
