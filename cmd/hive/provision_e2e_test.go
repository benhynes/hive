// End-to-end: `hive spawn --cwd` actually provisions the working directory —
// the wiring from the CLI/hub down to the provisioner. Runs standalone with an
// isolated HOME so it can never touch the real ~/.claude.json.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpawnProvisioning(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: daemon + tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// Isolate HOME: the provisioner writes ~/.claude.json, and the daemon
	// inherits this env, so the real user config is never touched.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Cleanup(func() { exec.Command("tmux", "-L", tmuxSocket, "kill-server").Run() })

	h := startHub(t, "provhost")
	out := mustCLI(t, h.env(), "net", "create", "dev")
	for _, m := range regexpTok.FindAllStringSubmatch(out, -1) {
		// tokens land in net.json; the hub env resolves control from there
		_ = m
	}

	cwd := filepath.Join(t.TempDir(), "agentwork")
	mustCLI(t, h.env(), "spawn", "--cwd", cwd, "worker", "--", "cat")

	// (1) .mcp.json landed in the agent's cwd with the hive server.
	b, err := os.ReadFile(filepath.Join(cwd, ".mcp.json"))
	if err != nil {
		t.Fatalf("spawn did not provision .mcp.json in cwd: %v", err)
	}
	var mcp struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &mcp); err != nil {
		t.Fatalf(".mcp.json invalid: %v", err)
	}
	hive, ok := mcp.MCPServers["hive"]
	if !ok || strings.Join(hive.Args, " ") != "mcp" {
		t.Fatalf("hive MCP server not provisioned correctly: %+v", mcp.MCPServers)
	}

	// (2) both Claude Code gates cleared for cwd in the isolated ~/.claude.json.
	cb, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("~/.claude.json not written: %v", err)
	}
	var claude struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool     `json:"hasTrustDialogAccepted"`
			EnabledMcpjsonServers  []string `json:"enabledMcpjsonServers"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(cb, &claude); err != nil {
		t.Fatalf("~/.claude.json invalid: %v", err)
	}
	abs, _ := filepath.Abs(cwd)
	entry, ok := claude.Projects[abs]
	if !ok {
		t.Fatalf("no trust preseed for %s: %+v", abs, claude.Projects)
	}
	if !entry.HasTrustDialogAccepted {
		t.Error("workspace-trust gate not cleared — a headless agent would hang on the trust prompt")
	}
	found := false
	for _, s := range entry.EnabledMcpjsonServers {
		if s == "hive" {
			found = true
		}
	}
	if !found {
		t.Errorf("hive not in enabledMcpjsonServers — the agent would hang on the MCP approval prompt: %v", entry.EnabledMcpjsonServers)
	}
}
