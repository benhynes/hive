// Package tmux wraps the tmux CLI for hive's control layer. All calls
// use exec arg vectors — no shell interpolation anywhere. Set
// HIVE_TMUX_SOCKET to use a dedicated server (tests do).
package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func args(rest ...string) []string {
	if sock := os.Getenv("HIVE_TMUX_SOCKET"); sock != "" {
		return append([]string{"-L", sock}, rest...)
	}
	return rest
}

func run(rest ...string) (string, error) {
	out, err := exec.Command("tmux", args(rest...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %v: %s", rest[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Available verifies tmux exists and is ≥ 3.2 (needed for new-session -e).
func Available() error {
	out, err := exec.Command("tmux", "-V").Output()
	if err != nil {
		return fmt.Errorf("tmux not found: %w", err)
	}
	v := regexp.MustCompile(`(\d+)\.(\d+)`).FindStringSubmatch(string(out))
	if v == nil {
		return nil // "tmux next-3.x" etc — assume fine
	}
	major, _ := strconv.Atoi(v[1])
	minor, _ := strconv.Atoi(v[2])
	if major < 3 || (major == 3 && minor < 2) {
		return fmt.Errorf("tmux %s too old (need ≥ 3.2 for -e)", strings.TrimSpace(string(out)))
	}
	return nil
}

// shellQuote makes s safe as a single word in a POSIX shell command.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// NewSession starts a detached session running cmd with env injected via
// tmux -e flags (values never pass through a shell). Returns the pane id.
func NewSession(session, cwd string, env map[string]string, cmd []string) (string, error) {
	a := []string{"new-session", "-d", "-s", session, "-x", "220", "-y", "50"}
	if cwd != "" {
		a = append(a, "-c", cwd)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a = append(a, "-e", k+"="+env[k])
	}
	quoted := make([]string, len(cmd))
	for i, c := range cmd {
		quoted[i] = shellQuote(c)
	}
	a = append(a, strings.Join(quoted, " "))
	if _, err := run(a...); err != nil {
		return "", err
	}
	return FirstPane(session)
}

// FirstPane returns the pane id (%N) of a session's first pane.
func FirstPane(session string) (string, error) {
	out, err := run("list-panes", "-t", session+":", "-F", "#{pane_id}")
	if err != nil {
		return "", err
	}
	pane := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if pane == "" {
		return "", fmt.Errorf("no pane in session %s", session)
	}
	return pane, nil
}

// SendKeysLiteral types text into a pane exactly as given (no key-name
// interpretation). Use Paste for multi-line text.
func SendKeysLiteral(pane, text string) error {
	_, err := run("send-keys", "-t", pane, "-l", "--", text)
	return err
}

// Enter presses Enter in a pane.
func Enter(pane string) error {
	_, err := run("send-keys", "-t", pane, "Enter")
	return err
}

// Paste inserts text (any bytes, any number of lines) via a tmux buffer
// with bracketed paste, which TUIs treat as a single paste event.
func Paste(pane, text string) error {
	c := exec.Command("tmux", args("load-buffer", "-b", "hive", "-")...)
	c.Stdin = strings.NewReader(text)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux load-buffer: %v: %s", err, strings.TrimSpace(string(out)))
	}
	_, err := run("paste-buffer", "-d", "-p", "-b", "hive", "-t", pane)
	return err
}

// Capture returns the visible pane content; lines > 0 also includes that
// much scrollback.
func Capture(pane string, lines int) (string, error) {
	a := []string{"capture-pane", "-p", "-t", pane}
	if lines > 0 {
		a = append(a, "-S", fmt.Sprintf("-%d", lines))
	}
	return run(a...)
}

// PaneExists reports whether the pane is still alive.
func PaneExists(pane string) bool {
	_, err := run("display-message", "-p", "-t", pane, "ok")
	return err == nil
}

// PanePID returns the pid of the process running in the pane.
func PanePID(pane string) (int, error) {
	out, err := run("display-message", "-p", "-t", pane, "#{pane_pid}")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// KillSession terminates a session and everything in it.
func KillSession(session string) error {
	_, err := run("kill-session", "-t", session)
	return err
}

// ProcStartEpoch returns a stable string identifying when pid started
// (`ps -o lstart=`, same format on macOS and Linux). Comparing it against
// the value captured at registration defeats pid reuse.
func ProcStartEpoch(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", fmt.Errorf("pid %d gone", pid)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("pid %d gone", pid)
	}
	return s, nil
}

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
// hot-spin tmux forks for the whole timeout.
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
