//go:build !windows

package control

// These tests exercise the control seam over the real tmux backend on a
// dedicated socket (the Windows backend can only be tested on Windows —
// see the e2e notes in docs/).

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func setup(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := fmt.Sprintf("hivectltest-%d", os.Getpid())
	t.Setenv("HIVE_TMUX_SOCKET", sock)
	t.Cleanup(func() {
		exec.Command("tmux", "-L", sock, "kill-server").Run()
	})
}

func TestLifecycleThroughSeam(t *testing.T) {
	setup(t)
	pane, spawnPID, err := NewSession("ctl-life", "", nil, []string{"sleep", "60"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !PaneExists(pane) {
		t.Fatal("pane should exist")
	}
	pid, err := PanePID(pane)
	if err != nil || pid <= 0 {
		t.Fatalf("pid=%d err=%v", pid, err)
	}
	if spawnPID != pid {
		t.Fatalf("NewSession pid %d != PanePID %d", spawnPID, pid)
	}
	epoch, err := ProcStartEpoch(pid)
	if err != nil || epoch == "" {
		t.Fatalf("epoch=%q err=%v", epoch, err)
	}
	if !Quiescent(pane, 100*time.Millisecond) {
		t.Fatal("sleeping pane should be quiescent")
	}
	if err := KillSession("ctl-life", pane); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for PaneExists(pane) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if PaneExists(pane) {
		t.Fatal("pane survived kill")
	}
}

func TestDuplicateSessionTyped(t *testing.T) {
	setup(t)
	if _, _, err := NewSession("ctl-dup", "", nil, []string{"sleep", "60"}, false); err != nil {
		t.Fatal(err)
	}
	_, _, err := NewSession("ctl-dup", "", nil, []string{"sleep", "60"}, false)
	if !errors.Is(err, ErrDuplicateSession) {
		t.Fatalf("want ErrDuplicateSession, got %v", err)
	}
	KillSession("ctl-dup", "")
}

// A dead pane must fail WaitQuiescent promptly instead of hot-spinning
// control forks until the deadline.
func TestWaitQuiescentDeadPane(t *testing.T) {
	setup(t)
	pane, _, err := NewSession("ctl-wq-dead", "", nil, []string{"cat"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := KillSession("ctl-wq-dead", pane); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if WaitQuiescent(pane, 100*time.Millisecond, 10*time.Second) {
		t.Fatal("dead pane reported quiescent")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("dead pane held WaitQuiescent for %v (want fast bail-out)", d)
	}
}
