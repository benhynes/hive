// End-to-end for SSH hosts (P2): register a host, and on first spawn have the
// origin hub bring up a transient daemon "over SSH" and forward the spawn to it
// through loopback port-forwards — then confirm the SSH-hosted agent is a full
// mesh peer (cross-hub messaging). The "remote" is this machine's loopback: an
// ssh shim (HIVE_SSH_BIN) runs remote commands locally and realizes -O
// forwards with socat, so the real internal/hub/sshhosts.go orchestration runs
// without a second machine.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSSHHostSpawnE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: daemons + tmux + socat")
	}
	for _, b := range []string{"tmux", "socat", "python3", "curl"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
	t.Cleanup(func() { exec.Command("tmux", "-L", tmuxSocket, "kill-server").Run() })

	home := t.TempDir()
	t.Setenv("HOME", home) // isolate the provisioner's ~/.claude.json

	shim, _ := filepath.Abs("testdata/sshshim.sh")
	os.Chmod(shim, 0o755)
	scp := filepath.Join(t.TempDir(), "scpshim")
	writeSCPShim(t, scp)
	t.Setenv("HIVE_SSH_BIN", shim)
	t.Setenv("HIVE_SCP_BIN", scp)
	t.Setenv("HIVE_SHIM_DIR", t.TempDir())

	origin := startHub(t, "origin")
	mustCLI(t, origin.env(), "net", "create", "dev")
	alice := register(t, origin, "alice")

	// Register the SSH host with a real free port for its transient daemon and
	// an isolated remote HIVE_HOME.
	remotePort := freePort(t)
	remoteHome := filepath.Join(t.TempDir(), "remote-hive")
	mustCLI(t, origin.env(), "hosts", "add-ssh",
		"--home", remoteHome, "--port", strconv.Itoa(remotePort), "edge", "me@localhost")

	// First spawn brings the host up (transient daemon + -L/-R tunnels) and
	// forwards the spawn. `cat` is a harmless long-lived runtime.
	spawnOut := mustCLI(t, origin.env(), "spawn",
		"--host", "edge", "--cwd", filepath.Join(t.TempDir(), "w"), "worker", "--", "cat")
	if !strings.Contains(spawnOut, "worker@edge") {
		t.Fatalf("spawn did not land on the SSH host: %q", spawnOut)
	}

	// The transient remote daemon is real — health-check it directly.
	if !eventually(6*time.Second, func() bool { return healthCheck(remotePort) }) {
		t.Fatalf("transient remote daemon never became healthy on :%d", remotePort)
	}

	// Discovery fan-out sees the SSH-hosted agent (proves the -L peer entry).
	if agents := mustCLI(t, origin.env(), "agents"); !strings.Contains(agents, "worker@edge") {
		t.Fatalf("SSH-hosted agent not visible in mesh discovery:\n%s", agents)
	}

	// Cross-hub delivery: origin agent -> SSH-hosted agent, over the -L tunnel.
	apiSend(t, alice, "worker@edge", "hello over the tunnel")
	inbox := filepath.Join(remoteHome, "nets", "dev", "inbox", "worker.jsonl")
	if !eventually(4*time.Second, func() bool {
		b, _ := os.ReadFile(inbox)
		return strings.Contains(string(b), "hello over the tunnel") && strings.Contains(string(b), "alice@origin")
	}) {
		b, _ := os.ReadFile(inbox)
		t.Fatalf("SSH-hosted agent did not receive cross-hub mail. inbox=%q", b)
	}

	// Teardown forgets the host and closes the tunnel.
	mustCLI(t, origin.env(), "hosts", "rm-ssh", "edge")
}

func writeSCPShim(t *testing.T, path string) {
	t.Helper()
	script := `#!/usr/bin/env bash
set -u
args=("$@"); n=${#args[@]}
local="${args[$((n-2))]}"; remote="${args[$((n-1))]}"; remote="${remote#*:}"
mkdir -p "$(dirname "$remote")"; cp "$local" "$remote"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func healthCheck(port int) bool {
	return exec.Command("curl", "-sf", "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/health").Run() == nil
}

func eventually(within time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}
