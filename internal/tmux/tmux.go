//go:build !windows

// Package tmux wraps the tmux CLI for hive's control layer on Unix
// (internal/control is the OS seam; Windows uses the console API). All
// calls use exec arg vectors — no shell interpolation anywhere. Set
// HIVE_TMUX_SOCKET to use a dedicated server (tests do).
package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
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

// CaptureRaw returns the visible pane content with escape sequences
// preserved (-e), followed by a cursor-position escape, so replaying it
// into a terminal emulator reproduces the screen. -N keeps trailing
// spaces so full-width TUI backgrounds survive.
func CaptureRaw(pane string) (string, error) {
	out, err := run("capture-pane", "-p", "-e", "-N", "-t", pane)
	if err != nil {
		return "", err
	}
	// capture-pane separates lines with bare \n; a terminal needs \r\n
	// or every line starts where the previous one ended (staircase).
	out = strings.ReplaceAll(out, "\n", "\r\n")
	pos, err := run("display-message", "-p", "-t", pane, "#{cursor_y} #{cursor_x}")
	if err != nil {
		return out, nil
	}
	var y, x int
	if _, err := fmt.Sscanf(strings.TrimSpace(pos), "%d %d", &y, &x); err == nil {
		out += fmt.Sprintf("\x1b[%d;%dH", y+1, x+1)
	}
	return out, nil
}

// PaneSize returns the pane's columns and rows.
func PaneSize(pane string) (cols, rows int, err error) {
	out, err := run("display-message", "-p", "-t", pane, "#{pane_width} #{pane_height}")
	if err != nil {
		return 0, 0, err
	}
	_, err = fmt.Sscanf(strings.TrimSpace(out), "%d %d", &cols, &rows)
	return cols, rows, err
}

// PipeOpen starts piping the pane's raw output into path (a FIFO the
// caller created). Replaces any existing pipe on the pane; tmux allows
// one pipe per pane.
func PipeOpen(pane, path string) error {
	_, err := run("pipe-pane", "-t", pane, "cat > "+shellQuote(path))
	return err
}

// PipeClose stops piping the pane's output.
func PipeClose(pane string) error {
	_, err := run("pipe-pane", "-t", pane)
	return err
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

// attachArgv is the command a human terminal runs to attach to session,
// honoring HIVE_TMUX_SOCKET.
func attachArgv(session string) []string {
	return append([]string{"tmux"}, args("attach-session", "-t", session)...)
}

// OpenWindow opens a terminal window on this host attached to the
// session — the "headed" spawn. Requires the daemon to run inside a
// GUI session (macOS user session, or DISPLAY/WAYLAND_DISPLAY set).
func OpenWindow(session string) error {
	argv := attachArgv(session)
	if runtime.GOOS == "darwin" {
		// Terminal.app via AppleScript: works without accessibility
		// permissions and reliably creates a visible window.
		script := fmt.Sprintf("tell application \"Terminal\"\nactivate\ndo script %q\nend tell",
			strings.Join(argv, " "))
		if out, err := exec.Command("osascript", "-e", script).CombinedOutput(); err != nil {
			return fmt.Errorf("osascript: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return fmt.Errorf("no DISPLAY/WAYLAND_DISPLAY in the daemon's environment")
	}
	type term struct {
		bin  string
		args []string
	}
	var candidates []term
	if t := os.Getenv("TERMINAL"); t != "" {
		candidates = append(candidates, term{t, []string{"-e"}})
	}
	candidates = append(candidates,
		term{"x-terminal-emulator", []string{"-e"}},
		term{"gnome-terminal", []string{"--"}},
		term{"konsole", []string{"-e"}},
		term{"kitty", []string{"-e"}},
		term{"alacritty", []string{"-e"}},
		term{"xterm", []string{"-e"}},
	)
	for _, c := range candidates {
		if _, err := exec.LookPath(c.bin); err != nil {
			continue
		}
		cmd := exec.Command(c.bin, append(c.args, argv...)...)
		if err := cmd.Start(); err != nil {
			continue
		}
		go cmd.Wait() // reap when the window closes
		return nil
	}
	return fmt.Errorf("no terminal emulator found (tried $TERMINAL, x-terminal-emulator, gnome-terminal, konsole, kitty, alacritty, xterm)")
}

// ProcStartEpoch returns a stable string identifying when pid started
// (`ps -o lstart=`). Comparing it against the value captured at
// registration defeats pid reuse.
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
