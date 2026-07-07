// Package control is the seam between hive's hub and the per-OS session
// control mechanism: tmux on Unix, the classic console API on Windows.
// The exported functions mirror what the hub needs — spawn, liveness,
// keys, screen capture, kill — and each platform file supplies them.
package control

import (
	"errors"
	"time"
)

// ErrDuplicateSession reports a spawn that collided with an existing
// session name. Only tmux can produce it (flat session namespace);
// Windows consoles have no shared namespace to collide in.
var ErrDuplicateSession = errors.New("duplicate session")

// Quiescent reports whether the pane's content is unchanged across the
// window — the readiness/idleness heuristic.
func Quiescent(pane string, window time.Duration) bool {
	a, err := Capture(pane, 0)
	if err != nil {
		return false
	}
	time.Sleep(window)
	b, err := Capture(pane, 0)
	return err == nil && a == b
}

// WaitQuiescent polls until the pane goes quiet or the deadline passes.
// A vanished pane returns false immediately — without the existence
// check a dead pane would make Quiescent fail before its sleep and
// hot-spin control forks for the whole timeout.
func WaitQuiescent(pane string, window, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !PaneExists(pane) {
			return false
		}
		if Quiescent(pane, window) {
			return true
		}
		time.Sleep(50 * time.Millisecond) // guard against error-path spins
	}
	return false
}
