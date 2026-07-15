// Does an idle agent reliably learn it has mail?
//
// The nudge is the ONLY thing that wakes an idle TUI agent, and it fires only
// as a side effect of delivery. This test asks whether a message that arrives
// inside the coalescing window is ever announced.
package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// paneText returns what is currently on the agent's screen.
func paneText(t *testing.T, env []string, agent string) string {
	t.Helper()
	return mustCLI(t, env, "read", agent)
}

// countNudges counts how many times the hub typed a mail nudge into the pane.
// Every nudge — preview or fallback — starts with the "hive: " marker followed
// by the sender, so the sender-tagged form is the reliable thing to count.
func countNudges(screen string) int {
	return strings.Count(screen, "sender@nudgehost")
}

func TestIdleAgentIsToldAboutEveryMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	t.Cleanup(func() { exec.Command("tmux", "-L", tmuxSocket, "kill-server").Run() })

	h := startHub(t, "nudgehost")
	out := mustCLI(t, h.env(), "net", "create", "dev")
	for _, m := range regexpTok.FindAllStringSubmatch(out, -1) {
		if m[1] == "msg" {
			h.msgTok = m[2]
		} else {
			h.ctlTok = m[2]
		}
	}
	sender := register(t, h, "sender")

	// A real tmux-bound agent, idle at a shell prompt — the pane exists, so it
	// is nudgeable, and it never polls its own inbox.
	mustCLI(t, h.env(), "spawn", "idle", "--", "sh")
	time.Sleep(500 * time.Millisecond)

	// First message: the agent is idle and un-nudged, so this must announce,
	// and the announcement must carry the sender + a body preview so the agent
	// can act without a round trip through recv.
	apiSend(t, sender, "idle", "first message")
	time.Sleep(1 * time.Second)
	screen := paneText(t, h.env(), "idle")
	if n := countNudges(screen); n != 1 {
		t.Fatalf("first message: got %d nudges on screen, want 1:\n%s", n, screen)
	}
	if !strings.Contains(screen, "sender@nudgehost") || !strings.Contains(screen, "first message") {
		t.Errorf("nudge should carry sender + body preview, got:\n%s", screen)
	}

	// Second message, well inside the coalescing window, and no further traffic
	// afterwards. The agent is still idle and now has UNREAD mail. Before the
	// re-arm fix this stalled forever: maybeNudge ran only on delivery, so with
	// no later delivery the second message was never announced.
	apiSend(t, sender, "idle", "second message")

	// Long enough for a couple of sweeper passes.
	time.Sleep(6 * time.Second)

	screen = paneText(t, h.env(), "idle")
	n := countNudges(screen)
	t.Logf("after 2 messages + 6s idle, nudges on screen = %d", n)

	// The sweeper must re-announce the still-unread second message.
	if n < 2 {
		t.Errorf("STALL: agent holds unread mail but was nudged only %d time(s); "+
			"the sweeper should have re-armed:\n%s", n, screen)
	}
}
