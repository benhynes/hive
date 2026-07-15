package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/benhynes/hive/internal/client"
	"github.com/benhynes/hive/internal/proto"
)

func TestAutomaticRunName(t *testing.T) {
	name := automaticRunName("/opt/tools/My Agent")
	if !proto.ValidName(name) {
		t.Fatalf("automatic name %q is invalid", name)
	}
	if !strings.HasPrefix(name, "myagent-") {
		t.Fatalf("automatic name %q does not identify the command", name)
	}
	if other := automaticRunName("/opt/tools/My Agent"); other == name {
		t.Fatalf("automatic names collided: %q", name)
	}

	long := automaticRunName("/usr/local/bin/abcdefghijklmnopqrstuvwxyz0123456789")
	if len(long) > 32 || !proto.ValidName(long) {
		t.Fatalf("long automatic name %q is invalid", long)
	}
}

func TestRunEnvironmentReplacesIdentityAndStripsControl(t *testing.T) {
	c := &client.Client{Addr: "http://hub:7777", Net: "dev"}
	reg := client.RegisterResp{Agent: "alice@local", Token: "personal"}
	env := runEnvironment([]string{
		"PATH=/bin",
		"HIVE_ADDR=http://old",
		"HIVE_NET=old",
		"HIVE_AGENT=parent@local",
		"HIVE_TOKEN=parent-token",
		"HIVE_CONTROL_TOKEN=control-secret",
		"HIVE_CONTROL_HOST=local",
	}, c, reg)

	got := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	for key, want := range map[string]string{
		"PATH": "/bin", "HIVE_ADDR": c.Addr, "HIVE_NET": c.Net,
		"HIVE_AGENT": reg.Agent, "HIVE_TOKEN": reg.Token,
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	for _, key := range []string{"HIVE_CONTROL_TOKEN", "HIVE_CONTROL_HOST"} {
		if _, ok := got[key]; ok {
			t.Errorf("message-only child inherited %s", key)
		}
	}
}

type recordingHeartbeater struct {
	mu    sync.Mutex
	calls int
	err   error
	wake  chan struct{}
}

func (h *recordingHeartbeater) HeartbeatContext(context.Context) (client.HeartbeatResp, error) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
	return client.HeartbeatResp{}, h.err
}

func TestHeartbeatUntilRunsAndCoalescesErrors(t *testing.T) {
	h := &recordingHeartbeater{err: errors.New("hub unavailable"), wake: make(chan struct{}, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	var errOut bytes.Buffer
	go func() {
		heartbeatUntil(ctx, h, time.Millisecond, &errOut)
		close(returned)
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-h.wake:
		case <-time.After(time.Second):
			t.Fatal("heartbeat loop did not call client")
		}
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("heartbeat loop did not stop")
	}
	if count := strings.Count(errOut.String(), "hub unavailable"); count != 1 {
		t.Fatalf("heartbeat error printed %d times, want once: %q", count, errOut.String())
	}
}

type blockingHeartbeater struct {
	started chan struct{}
}

func (h blockingHeartbeater) HeartbeatContext(ctx context.Context) (client.HeartbeatResp, error) {
	close(h.started)
	<-ctx.Done()
	return client.HeartbeatResp{}, ctx.Err()
}

func TestHeartbeatUntilCancelsInFlightRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		heartbeatUntil(ctx, blockingHeartbeater{started: started}, time.Millisecond, io.Discard)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("heartbeat request never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat loop did not cancel its in-flight request")
	}
}

func TestSignalRunProcessStopsDescendants(t *testing.T) {
	if !runUsesProcessGroup {
		t.Skip("platform uses direct child signaling")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("sh", "-c", `sleep 30 & echo $! > "$1"; wait`, "sh", pidFile)
	restore := prepareRunProcess(cmd, nil)
	defer restore()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_ = cmd.Process.Kill()
		if !waited {
			_ = cmd.Wait()
		}
	}()

	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(b)))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("shell did not report its sleep child")
	}

	signalRunProcess(cmd.Process, syscall.SIGTERM)
	_ = cmd.Wait()
	waited = true
	deadline = time.Now().Add(2 * time.Second)
	for processRunning(childPID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processRunning(childPID) {
		t.Fatalf("descendant pid %d survived wrapper signal", childPID)
	}
}

func processRunning(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(out))
	return state != "" && !strings.HasPrefix(state, "Z")
}

func requireRunAgentState(t *testing.T, raw, agent string, wantAlive, wantEphemeral bool) {
	t.Helper()
	var roster client.AgentsResp
	if err := json.Unmarshal([]byte(raw), &roster); err != nil {
		t.Fatalf("decode agents: %v\n%s", err, raw)
	}
	for _, got := range roster.Agents {
		if got.Agent == agent {
			if got.Alive != wantAlive || got.Ephemeral != wantEphemeral {
				t.Fatalf("agent %s state=%+v, want alive=%v ephemeral=%v", agent, got, wantAlive, wantEphemeral)
			}
			return
		}
	}
	t.Fatalf("agent %s missing from roster: %s", agent, raw)
}

func TestRunEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: daemon + attached subprocess")
	}
	if runtime.GOOS == "windows" {
		t.Skip("test child uses sh and pwd")
	}
	h := startHub(t, "runhost")
	mustCLI(t, h.env(), "net", "create", "dev")
	cwd := t.TempDir()

	t.Run("injects-message-identity-and-goes-offline", func(t *testing.T) {
		script := `printf '%s\n' "$HIVE_ADDR" "$HIVE_NET" "$HIVE_AGENT" "$HIVE_TOKEN" "${HIVE_CONTROL_TOKEN-unset}" "${HIVE_CONTROL_HOST-unset}"; pwd`
		cmd := exec.Command(hiveBin(t), "run", "--name", "alice", "--cwd", cwd, "--", "sh", "-c", script)
		cmd.Env = h.env(
			"HIVE_AGENT=parent@runhost", "HIVE_TOKEN="+strings.Repeat("c", 64),
			"HIVE_CONTROL_TOKEN=must-not-leak", "HIVE_CONTROL_HOST=runhost",
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("hive run: %v: %s", err, stderr.String())
		}
		lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
		if len(lines) != 7 {
			t.Fatalf("child output has %d lines, want 7: %q", len(lines), stdout.String())
		}
		if lines[0] != h.url() || lines[1] != "dev" || lines[2] != "alice@runhost" {
			t.Fatalf("bad injected identity: %q", lines[:3])
		}
		if ok, _ := regexp.MatchString(`^[0-9a-f]{64}$`, lines[3]); !ok {
			t.Fatalf("bad personal token %q", lines[3])
		}
		if lines[4] != "unset" || lines[5] != "unset" {
			t.Fatalf("control environment leaked: %q", lines[4:6])
		}
		if lines[6] != filepath.Clean(cwd) {
			t.Fatalf("child cwd = %q, want %q", lines[6], cwd)
		}
		if !strings.Contains(stderr.String(), "alice@runhost") {
			t.Fatalf("launcher did not announce identity: %q", stderr.String())
		}
		agents := mustCLI(t, h.env(), "agents", "--local", "--json")
		requireRunAgentState(t, agents, "alice@runhost", false, false)
	})

	t.Run("preserves-child-exit-status", func(t *testing.T) {
		cmd := exec.Command(hiveBin(t), "run", "--name", "failing", "--", "sh", "-c", "exit 23")
		cmd.Env = h.env()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
			t.Fatalf("exit = %v, want status 23 (stderr %q)", err, stderr.String())
		}
		if strings.Contains(stderr.String(), "hive: child exited") {
			t.Fatalf("launcher rendered child status as its own error: %q", stderr.String())
		}
		agents := mustCLI(t, h.env(), "agents", "--local", "--json")
		requireRunAgentState(t, agents, "failing@runhost", false, false)
	})

	t.Run("maps-signaled-child-exit-status", func(t *testing.T) {
		cmd := exec.Command(hiveBin(t), "run", "--name", "signaled", "--", "sh", "-c", "kill -TERM $$")
		cmd.Env = h.env()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128+int(syscall.SIGTERM) {
			t.Fatalf("exit = %v, want status %d (stderr %q)", err, 128+int(syscall.SIGTERM), stderr.String())
		}
		agents := mustCLI(t, h.env(), "agents", "--local", "--json")
		requireRunAgentState(t, agents, "signaled@runhost", false, false)
	})

	t.Run("forwards-direct-sigint-with-tty-independent-semantics", func(t *testing.T) {
		ready := filepath.Join(t.TempDir(), "child-ready")
		cmd := exec.Command(hiveBin(t), "run", "--name", "interrupted", "--", "sh", "-c", `trap 'exit 42' INT; : > "$1"; while :; do :; done`, "sh", ready)
		cmd.Env = h.env()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatalf("hive run child was not ready before SIGINT: %s", stderr.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		err := cmd.Wait()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
			t.Fatalf("direct SIGINT exit = %v, want child trap status 42 (stderr %q)", err, stderr.String())
		}
		agents := mustCLI(t, h.env(), "agents", "--local", "--json")
		requireRunAgentState(t, agents, "interrupted@runhost", false, false)
	})

	t.Run("generated-identity-is-disposable", func(t *testing.T) {
		cmd := exec.Command(hiveBin(t), "run", "--", "sh", "-c", `printf '%s' "$HIVE_AGENT"`)
		cmd.Env = h.env()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("generated hive run: %v: %s", err, stderr.String())
		}
		agent := strings.TrimSpace(string(out))
		if agent == "" {
			t.Fatal("generated run did not inject HIVE_AGENT")
		}
		agents := mustCLI(t, h.env(), "agents", "--local", "--json")
		if strings.Contains(agents, agent) {
			t.Fatalf("generated identity remained after child exit: %s", agents)
		}
	})
}
