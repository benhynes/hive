//go:build !windows

package control

import (
	"fmt"
	"strings"

	"github.com/benhynes/hive/internal/tmux"
)

// Available verifies the platform control mechanism is usable.
func Available() error { return tmux.Available() }

// AllowClientPane reports whether a client may self-register the given
// pane id. tmux pane ids (%N) are only valid for sessions this server
// owns, so a client cannot forge one pointing at an unrelated process —
// PaneExists rejects anything tmux did not create. Always permitted.
func AllowClientPane(pane string) error { return nil }

// NewSession starts a detached session running cmd with env injected and
// returns its pane id and pid. headed is a spawn-time hint only Windows
// needs (tmux sessions are always headless; OpenWindow attaches a terminal
// afterwards).
func NewSession(session, cwd string, env map[string]string, cmd []string, headed bool) (string, int, error) {
	pane, pid, err := tmux.NewSession(session, cwd, env, cmd)
	if err != nil && strings.Contains(err.Error(), "duplicate session") {
		return "", 0, fmt.Errorf("%w: %v", ErrDuplicateSession, err)
	}
	return pane, pid, err
}

// PaneExists reports whether the pane is still alive.
func PaneExists(pane string) bool { return tmux.PaneExists(pane) }

// PanePID returns the pid of the process bound to the pane.
func PanePID(pane string) (int, error) { return tmux.PanePID(pane) }

// SendKeysLiteral types text into a pane exactly as given.
func SendKeysLiteral(pane, text string) error { return tmux.SendKeysLiteral(pane, text) }

// Enter presses Enter in a pane.
func Enter(pane string) error { return tmux.Enter(pane) }

// Paste inserts multi-line text as a single paste event.
func Paste(pane, text string) error { return tmux.Paste(pane, text) }

// Capture returns the visible pane content; lines > 0 also includes that
// much scrollback.
func Capture(pane string, lines int) (string, error) { return tmux.Capture(pane, lines) }

// KillSession terminates a session and everything in it. The pane id is
// unused on Unix (tmux kills by session name).
func KillSession(session, _ string) error { return tmux.KillSession(session) }

// OpenWindow opens a visible terminal on this host attached to the
// session — the "headed" spawn. The pane id is unused on Unix.
func OpenWindow(session, _ string) error { return tmux.OpenWindow(session) }

// ProcStartEpoch returns a stable string identifying when pid started;
// comparing it against the value captured at registration defeats pid
// reuse.
func ProcStartEpoch(pid int) (string, error) { return tmux.ProcStartEpoch(pid) }

// RunConOp is the hidden Windows console-op helper; it never runs on Unix.
func RunConOp(args []string) error {
	return fmt.Errorf("__conop is a Windows-only internal command")
}
