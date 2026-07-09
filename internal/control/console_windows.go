//go:build windows

// Windows control backend. tmux does not exist here; the equivalent
// mechanism is the classic console API, validated empirically on real
// Windows (see docs/windows-control.md): CreateProcess gives each spawn
// its own conhost, AttachConsole + CONOUT$/CONIN$ read the rendered
// screen grid and inject keystrokes, OpenProcess answers liveness.
//
// A process can attach to only one console at a time and FreeConsole
// would invalidate the daemon's own stdio, so every attach-requiring op
// (capture/keys/show) runs in a short-lived helper: the daemon re-execs
// itself as `hive __conop <op> <pid> <epoch> ...` with DETACHED_PROCESS
// (see conop_windows.go). That mirrors tmux's exec-per-op shape and
// keeps console signals (a stray CTRL_C_EVENT while attached) away from
// the daemon. Spawn, liveness, and kill need no console and run
// in-process.
//
// Pane ids are "win:<pid>:<creation-filetime>" — the pid owning the
// session's console plus its process-creation timestamp. Every op that
// touches a pid first re-opens the process and confirms the creation
// time still matches, holding that handle across the operation; an open
// handle pins the pid so Windows cannot recycle it mid-op, and the
// timestamp check rejects a pid that was already reused (e.g. across a
// daemon restart, when no handle was held). This defeats the pid-reuse
// race where capture/keys/kill would otherwise hit an unrelated process.
package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	createNewConsole    = 0x00000010
	createNoWindow      = 0x08000000
	detachedProcess     = 0x00000008
	startfUseCountChars = 0x00000008
	processQueryLimited = 0x00001000 // PROCESS_QUERY_LIMITED_INFORMATION
	waitTimeout         = 0x00000102 // WAIT_TIMEOUT

	// Console geometry, mirroring the tmux backend's new-session
	// -x 220 -y 50 with a 2000-line history. The buffer is the window
	// plus scrollback; conhost caps the window height to screen metrics,
	// so the window rect is whatever conhost grants.
	bufWidth  = 220
	bufHeight = 2050

	conOpTimeout = 10 * time.Second
)

// paneLocks serializes console ops per pane within this daemon process.
// Two helpers may legally attach to one console at once and interleave
// WriteConsoleInput records or read mid-write; tmux serializes through
// its server, and we serialize here.
//
// Each entry is reference-counted and dropped when its last holder
// releases, so the map is bounded by the number of panes with an op
// in flight — not by the total spawns over the daemon's lifetime
// (every spawn mints a unique win:<pid>:<creation> pane).
type paneLock struct {
	mu   sync.Mutex
	refs int
}

var (
	paneLocksMu sync.Mutex
	paneLocks   = map[string]*paneLock{}
)

func lockPane(pane string) func() {
	paneLocksMu.Lock()
	pl := paneLocks[pane]
	if pl == nil {
		pl = &paneLock{}
		paneLocks[pane] = pl
	}
	pl.refs++ // pin the entry so a concurrent releaser can't evict it
	paneLocksMu.Unlock()

	pl.mu.Lock() // block outside paneLocksMu — a console op can be slow
	return func() {
		pl.mu.Unlock()
		paneLocksMu.Lock()
		if pl.refs--; pl.refs == 0 {
			delete(paneLocks, pane)
		}
		paneLocksMu.Unlock()
	}
}

// Available reports whether control ops can work here. The console API
// ships with Windows itself, so there is nothing to check.
func Available() error { return nil }

// AllowClientPane rejects a client-supplied pane id. A win:<pid> pane
// names an arbitrary process by pid, so honoring one from a
// self-registering client would let a net-token holder bind a
// controllable agent onto any process (then keys/kill it via a
// control-token holder). Windows panes are only ever assigned
// server-side by a spawn; a legitimate Windows agent registers with
// --pid (liveness only, not controllable) or lets the hub spawn it.
func AllowClientPane(pane string) error {
	if pane != "" {
		return fmt.Errorf("Windows nodes do not accept a client-supplied pane; register with --pid or spawn via the hub")
	}
	return nil
}

func winPane(pid uint32, creation uint64) string {
	return fmt.Sprintf("win:%d:%d", pid, creation)
}

// parsePane splits "win:<pid>" or "win:<pid>:<creation>". A pane without
// a creation stamp (hasEpoch=false) can still be spawned by an agent's
// self-registration, where only the pid is known; its ops skip the
// timestamp check and fall back to a bare liveness probe.
func parsePane(pane string) (pid uint32, creation uint64, hasEpoch bool, err error) {
	rest, ok := strings.CutPrefix(pane, "win:")
	if !ok {
		return 0, 0, false, fmt.Errorf("bad pane id %q (want win:<pid>)", pane)
	}
	pidStr := rest
	epochStr := ""
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		pidStr, epochStr = rest[:i], rest[i+1:]
	}
	p, err := strconv.ParseUint(pidStr, 10, 32)
	if err != nil || p == 0 {
		return 0, 0, false, fmt.Errorf("bad pane id %q (want win:<pid>)", pane)
	}
	if epochStr != "" {
		c, err := strconv.ParseUint(epochStr, 10, 64)
		if err != nil {
			return 0, 0, false, fmt.Errorf("bad pane id %q (bad creation stamp)", pane)
		}
		return uint32(p), c, true, nil
	}
	return uint32(p), 0, false, nil
}

// creationTime reads a process handle's creation FILETIME as a uint64.
func creationTime(h syscall.Handle) (uint64, error) {
	var c, e, k, u syscall.Filetime
	if err := syscall.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0, err
	}
	return uint64(c.HighDateTime)<<32 | uint64(c.LowDateTime), nil
}

// openVerified opens the pid with `access` and, when the pane carries a
// creation stamp, confirms it still matches — proving the pid was not
// reused. The returned handle pins the pid against reuse until closed;
// the caller MUST close it. Errors if the process is gone or replaced.
func openVerified(pid uint32, creation uint64, hasEpoch bool, access uint32) (syscall.Handle, error) {
	h, err := syscall.OpenProcess(access|processQueryLimited, false, pid)
	if err != nil {
		return 0, fmt.Errorf("pid %d gone", pid)
	}
	if hasEpoch {
		got, err := creationTime(h)
		if err != nil {
			syscall.CloseHandle(h)
			return 0, fmt.Errorf("pid %d gone", pid)
		}
		if got != creation {
			syscall.CloseHandle(h)
			return 0, fmt.Errorf("pid %d was reused (creation stamp mismatch)", pid)
		}
	}
	return h, nil
}

// NewSession starts cmd in its own console with env injected and returns
// the pane id and pid. headed selects a visible console window (only
// meaningful when the daemon runs in the interactive user session);
// headless spawns use CREATE_NO_WINDOW — same conhost, no window, still
// controllable.
func NewSession(session, cwd string, env map[string]string, cmd []string, headed bool) (string, int, error) {
	if len(cmd) == 0 {
		return "", 0, fmt.Errorf("empty command")
	}
	path, err := exec.LookPath(cmd[0])
	if err != nil && !errors.Is(err, exec.ErrDot) {
		return "", 0, fmt.Errorf("%s: %w", cmd[0], err)
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", 0, err
	}
	argv := append([]string{path}, cmd[1:]...)
	// CreateProcess cannot exec batch files directly; hand them to
	// cmd.exe. Caveat: cmd.exe re-parses the line, so batch targets with
	// metacharacter-laden args may misparse — unavoidable on Windows.
	if e := strings.ToLower(filepath.Ext(path)); e == ".bat" || e == ".cmd" {
		comspec := os.Getenv("ComSpec")
		if comspec == "" {
			comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
		}
		argv = append([]string{comspec, "/d", "/c"}, argv...)
		path = comspec
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = syscall.EscapeArg(a)
	}
	cmdline, err := syscall.UTF16PtrFromString(strings.Join(quoted, " "))
	if err != nil {
		return "", 0, fmt.Errorf("command contains NUL")
	}
	// lpApplicationName is set explicitly (not derived from the command
	// line) so CreateProcess never runs its ambiguous first-token search,
	// which would otherwise probe the current directory.
	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", 0, fmt.Errorf("command contains NUL")
	}
	var dirp *uint16
	if cwd != "" {
		if dirp, err = syscall.UTF16PtrFromString(cwd); err != nil {
			return "", 0, fmt.Errorf("cwd contains NUL")
		}
	}
	block, err := envBlock(env)
	if err != nil {
		return "", 0, err
	}

	si := &syscall.StartupInfo{
		Flags:       startfUseCountChars,
		XCountChars: bufWidth,
		YCountChars: bufHeight,
	}
	si.Cb = uint32(unsafe.Sizeof(*si))
	pi := new(syscall.ProcessInformation)
	flags := uint32(syscall.CREATE_UNICODE_ENVIRONMENT)
	if headed {
		flags |= createNewConsole
	} else {
		flags |= createNoWindow
	}
	if err := syscall.CreateProcess(pathp, cmdline, nil, nil, false, flags, &block[0], dirp, si, pi); err != nil {
		return "", 0, fmt.Errorf("CreateProcess %s: %v", cmd[0], err)
	}
	syscall.CloseHandle(pi.Thread)
	// Read the creation stamp from the process handle we already hold,
	// before releasing it, so the pane id is self-verifying from here on.
	creation, cerr := creationTime(pi.Process)
	syscall.CloseHandle(pi.Process)
	if cerr != nil {
		return "", 0, fmt.Errorf("spawned process died immediately")
	}
	return winPane(pi.ProcessId, creation), int(pi.ProcessId), nil
}

// envBlock merges extra into the daemon's environment (Windows env names
// are case-insensitive; extra wins) and encodes the sorted UTF-16
// double-NUL block CreateProcess wants with CREATE_UNICODE_ENVIRONMENT.
func envBlock(extra map[string]string) ([]uint16, error) {
	type kv struct{ k, v string }
	merged := map[string]kv{} // upper-cased name → original pair
	for _, s := range os.Environ() {
		// Per-drive cwd entries look like "=C:=C:\dir": an empty name,
		// then '=' at index 0. Search for the separator from index 1 so
		// those are split as name "=C:" and preserved verbatim rather
		// than mangled into an empty key.
		i := strings.IndexByte(s[1:], '=')
		if i < 0 {
			continue
		}
		k, v := s[:i+1], s[i+2:]
		merged[strings.ToUpper(k)] = kv{k, v}
	}
	for k, v := range extra {
		merged[strings.ToUpper(k)] = kv{k, v}
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys) // case-insensitive: keys are upper-cased
	var block []uint16
	for _, k := range keys {
		p := merged[k]
		u, err := syscall.UTF16FromString(p.k + "=" + p.v)
		if err != nil {
			return nil, fmt.Errorf("env %s contains NUL", p.k)
		}
		block = append(block, u...)
	}
	block = append(block, 0) // double-NUL terminator
	return block, nil
}

// paneAlive verifies the pane's process is present and, when the pane
// carries a creation stamp, that it is the same process (not a reused
// pid).
func paneAlive(pane string) bool {
	pid, creation, hasEpoch, err := parsePane(pane)
	if err != nil {
		return false
	}
	h, err := openVerified(pid, creation, hasEpoch, syscall.SYNCHRONIZE)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	ev, werr := syscall.WaitForSingleObject(h, 0)
	return werr == nil && ev == waitTimeout
}

// PaneExists reports whether the pane's process is still alive and
// unreplaced.
func PaneExists(pane string) bool { return paneAlive(pane) }

// PanePID returns the pid encoded in the pane id, verifying it is alive
// and unreplaced.
func PanePID(pane string) (int, error) {
	pid, _, _, err := parsePane(pane)
	if err != nil {
		return 0, err
	}
	if !paneAlive(pane) {
		return 0, fmt.Errorf("pane %s: process gone", pane)
	}
	return int(pid), nil
}

// ProcStartEpoch returns the process creation FILETIME as a decimal
// string. Comparing it against the value captured at registration
// defeats pid reuse. (The pane id embeds the same fact for spawned
// sessions; this serves the registry's per-pid StartEpoch field.)
func ProcStartEpoch(pid int) (string, error) {
	h, err := syscall.OpenProcess(processQueryLimited, false, uint32(pid))
	if err != nil {
		return "", fmt.Errorf("pid %d gone", pid)
	}
	defer syscall.CloseHandle(h)
	c, err := creationTime(h)
	if err != nil {
		return "", fmt.Errorf("pid %d gone", pid)
	}
	return fmt.Sprintf("ft:%d", c), nil
}

// KillSession terminates the pane's process and its whole tree (tmux
// kill-session parity). The session name is display-only on Windows.
func KillSession(_, pane string) error {
	pid, creation, hasEpoch, err := parsePane(pane)
	if err != nil {
		return err
	}
	// Tear the helper down BEFORE contending for lockPane: if a conOp is
	// wedged in a helper exchange holding lockPane, killing the helper makes
	// its request() read fail so that conOp releases the lock — otherwise
	// KillSession could never acquire it and the pane would be un-killable.
	killHelper(pane)
	unlock := lockPane(pane)
	defer unlock()
	// Holding this handle pins the pid: taskkill /PID re-resolves by pid,
	// but Windows cannot recycle a pid while a handle to it is open, so
	// the resolve is guaranteed to hit our verified process.
	h, err := openVerified(pid, creation, hasEpoch, syscall.PROCESS_TERMINATE)
	if err != nil {
		return fmt.Errorf("pane %s: %v", pane, err)
	}
	defer syscall.CloseHandle(h)

	// Bound taskkill the same way conOp bounds its helper: it runs while
	// the pane mutex is held, so a hung taskkill (a wedged /F on a
	// process stuck in an uninterruptible wait) would otherwise wedge
	// every future console op on this pane. On timeout, fall through to
	// the pinned-handle TerminateProcess below.
	ctx, cancel := context.WithTimeout(context.Background(), conOpTimeout)
	defer cancel()
	taskkill := filepath.Join(os.Getenv("SystemRoot"), "System32", "taskkill.exe")
	out, kerr := exec.CommandContext(ctx, taskkill, "/T", "/F", "/PID", strconv.Itoa(int(pid))).CombinedOutput()
	if kerr == nil {
		return nil
	}
	// Fallback: at least terminate the root process directly via the
	// pinned handle (breakaway grandchildren may survive — documented).
	if terr := syscall.TerminateProcess(h, 1); terr != nil {
		return fmt.Errorf("taskkill: %v: %s; TerminateProcess: %v", kerr, strings.TrimSpace(string(out)), terr)
	}
	return nil
}

// SendKeysLiteral types text into the pane's console exactly as given.
func SendKeysLiteral(pane, text string) error {
	_, err := conOp(pane, "keys", base64.StdEncoding.EncodeToString([]byte(text)))
	return err
}

// Enter presses Enter in the pane's console.
func Enter(pane string) error {
	_, err := conOp(pane, "keys", base64.StdEncoding.EncodeToString([]byte("\r")))
	return err
}

// Paste types multi-line text. conhost has no bracketed paste; lines are
// typed with carriage returns between them, which line-oriented programs
// treat as separate submissions (documented behavioral difference from
// the tmux backend).
func Paste(pane, text string) error {
	text = strings.ReplaceAll(text, "\r\n", "\r")
	text = strings.ReplaceAll(text, "\n", "\r")
	_, err := conOp(pane, "keys", base64.StdEncoding.EncodeToString([]byte(text)))
	return err
}

// Capture returns the console's visible window content; lines > 0 also
// includes that much scrollback from the buffer above the window.
func Capture(pane string, lines int) (string, error) {
	if lines < 0 {
		lines = 0
	}
	return conOp(pane, "capture", strconv.Itoa(lines))
}

// Live pane streaming needs tmux pipe-pane; the classic console API has
// no equivalent (polling ReadConsoleOutput is the only read primitive).
func StreamSupported() bool { return false }

func CaptureRaw(pane string) (string, error) {
	return "", fmt.Errorf("pane streaming is not supported on Windows")
}

func PaneSize(pane string) (cols, rows int, err error) {
	return 0, 0, fmt.Errorf("pane streaming is not supported on Windows")
}

func PipeOpen(pane, path string) error {
	return fmt.Errorf("pane streaming is not supported on Windows")
}

func PipeClose(pane string) error { return nil }

// OpenWindow makes the session's console window visible — only possible
// for sessions spawned with headed=true (CREATE_NO_WINDOW consoles have
// no window at all, revealable or otherwise).
//
// Right after CreateProcess the child's conhost has not necessarily
// created its console window yet, so the first show races and fails
// ("handle is invalid", or a NULL hwnd). A delayed show is observed to
// succeed within a couple of seconds, so retry with backoff — treating
// ANY error as retryable, since the race surfaces as both shapes.
func OpenWindow(_, pane string) error {
	backoff := 50 * time.Millisecond
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := conOp(pane, "show")
		if err == nil {
			return nil
		}
		// Permanent conditions (a headless spawn, or a non-interactive
		// Session-0 daemon) will never succeed — return at once instead of
		// spinning the whole retry budget. Only the conhost-not-ready race
		// after CreateProcess is transient and worth retrying.
		if s := err.Error(); strings.Contains(s, "no console window") ||
			strings.Contains(s, "interactive desktop session") {
			return err
		}
		if !time.Now().Add(backoff).Before(deadline) {
			return err
		}
		time.Sleep(backoff)
		if backoff < 800*time.Millisecond {
			backoff *= 2
		}
	}
}

// conHelper is a persistent `hive __conop serve` process bound to one
// pane's console: it AttachConsole's once, then services framed ops over
// its stdio pipes, so repeated keys/read/show skip the per-op CreateProcess
// + Go-runtime init + AttachConsole that the one-shot path pays. Access is
// serialized by the caller's lockPane(pane), so no internal mutex is
// needed; helpersMu only guards the map against other panes' teardowns.
type conHelper struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

var (
	helpersMu sync.Mutex
	helpers   = map[string]*conHelper{}
)

// conOp runs one attach-requiring console operation. It prefers the pane's
// persistent helper and falls back to a one-shot re-exec on a transport
// failure (no helper yet, or a dead one) — which also covers cold start.
//
// The fallback is gated for safety: a keys op that reached the helper but
// whose reply was lost may already have typed into the console, so re-running
// it would duplicate keystrokes. Only fall back when the op provably never
// reached the helper (write-side failure) or is read-only/idempotent
// (capture, show); otherwise surface the transport error.
func conOp(pane, op string, args ...string) (string, error) {
	if _, _, _, err := parsePane(pane); err != nil {
		return "", err
	}
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	unlock := lockPane(pane)
	defer unlock()

	if h, err := getHelperLocked(pane); err == nil {
		data, opErr, transportErr, wrote := h.request(op, arg)
		if transportErr == nil {
			return data, opErr
		}
		dropHelperLocked(pane, h)
		if wrote && op != "capture" && op != "show" {
			return "", fmt.Errorf("console %s: %v", op, transportErr)
		}
		// fall through to a one-shot for this op
	}
	return conOpOneShot(pane, op, arg)
}

// getHelperLocked returns the pane's helper, spawning one if absent. The
// caller must hold lockPane(pane), which serializes spawns for a pane.
func getHelperLocked(pane string) (*conHelper, error) {
	helpersMu.Lock()
	h := helpers[pane]
	helpersMu.Unlock()
	if h != nil {
		return h, nil
	}
	h, err := startHelper(pane)
	if err != nil {
		return nil, err
	}
	helpersMu.Lock()
	helpers[pane] = h
	helpersMu.Unlock()
	return h, nil
}

func startHelper(pane string) (*conHelper, error) {
	pid, creation, _, err := parsePane(pane)
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "__conop", "serve", strconv.Itoa(int(pid)), strconv.FormatUint(creation, 10))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// The serve loop attaches BEFORE reading any request, so a request sent
	// now simply waits in the pipe until attach completes; if attach fails
	// the process exits and the first request's read hits EOF (a transport
	// error the caller turns into a one-shot fallback).
	return &conHelper{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

// request sends one framed op and reads the framed reply, bounded by
// conOpTimeout so a wedged console (a helper stuck in a kernel console call
// that never replies) can't block the pane forever — the one-shot path had
// this bound and the fast path must not lose it. transportErr signals a
// broken/dead/wedged helper (pipe failure, timeout, malformed frame); opErr
// is a well-formed ERR reply from a healthy helper. wrote reports whether
// the request reached the helper's stdin, so the caller knows a mutating op
// may already have run (making a re-run unsafe).
//
// The write is synchronous: the request is tiny (< the pipe buffer), so it
// cannot block even if the helper is not draining stdin. Only the reply
// read can block, so only it is watchdogged.
func (h *conHelper) request(op, arg string) (data string, opErr, transportErr error, wrote bool) {
	if _, err := io.WriteString(h.stdin, op+"\t"+arg+"\n"); err != nil {
		return "", nil, err, false
	}
	type reply struct {
		line string
		err  error
	}
	ch := make(chan reply, 1)
	go func() {
		line, err := h.stdout.ReadString('\n')
		ch <- reply{line, err}
	}()
	select {
	case <-time.After(conOpTimeout):
		return "", nil, fmt.Errorf("helper timed out (console unresponsive)"), true
	case r := <-ch:
		if r.err != nil {
			return "", nil, r.err, true
		}
		status, b64, _ := strings.Cut(strings.TrimRight(r.line, "\r\n"), " ")
		raw, derr := base64.StdEncoding.DecodeString(b64)
		if derr != nil {
			return "", nil, fmt.Errorf("bad helper payload: %v", derr), true
		}
		switch status {
		case "OK":
			return string(raw), nil, nil, true
		case "ERR":
			return "", fmt.Errorf("console %s: %s", op, string(raw)), nil, true
		default:
			return "", nil, fmt.Errorf("bad helper status %q", status), true
		}
	}
}

// close terminates the helper. Killing the process (not just closing stdin)
// is required to unblock a helper wedged in a console call and to release
// its pinned pid handle and any leaked reply-reader goroutine.
func (h *conHelper) close() {
	h.stdin.Close()
	if h.cmd.Process != nil {
		h.cmd.Process.Kill()
	}
	go h.cmd.Wait() // reap the detached process
}

// dropHelperLocked removes a dead helper (caller holds lockPane(pane)).
func dropHelperLocked(pane string, h *conHelper) {
	helpersMu.Lock()
	if helpers[pane] == h {
		delete(helpers, pane)
	}
	helpersMu.Unlock()
	h.close()
}

// killHelper tears down a pane's helper on session kill.
func killHelper(pane string) {
	helpersMu.Lock()
	h := helpers[pane]
	delete(helpers, pane)
	helpersMu.Unlock()
	if h != nil {
		h.close()
	}
}

// conOpOneShot runs one op in a fresh detached `hive __conop` process (the
// cold-start / degraded-fallback path; the pid and creation stamp travel
// as argv so the helper re-verifies identity before it attaches).
func conOpOneShot(pane, op, arg string) (string, error) {
	pid, creation, _, err := parsePane(pane)
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), conOpTimeout)
	defer cancel()
	argv := []string{"__conop", op, strconv.Itoa(int(pid)), strconv.FormatUint(creation, 10)}
	if arg != "" {
		argv = append(argv, arg)
	}
	cmd := exec.CommandContext(ctx, exe, argv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() != nil {
			msg = "helper timed out (console unresponsive)"
		}
		return "", fmt.Errorf("console %s: %s", op, msg)
	}
	return stdout.String(), nil
}
