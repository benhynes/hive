//go:build windows

package control

// The `hive __conop` helper: every console operation that needs
// AttachConsole runs here, in a process the daemon spawns per op with
// DETACHED_PROCESS (no console of its own, so the attach cannot
// collide) and pipes for stdio (unaffected by console attach/detach).
// The probe run that validated non-parent AttachConsole + grid reads +
// key injection on real Windows is described in docs/windows-control.md.
//
// The helper re-verifies the target's identity before attaching: it
// opens the pid and confirms the creation FILETIME matches the stamp the
// daemon passed, keeping that handle open across the whole op. An open
// handle pins the pid, so even if the target exits mid-op Windows cannot
// recycle its pid into an unrelated process that we would then read from
// or type into.

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	k32                       = syscall.NewLazyDLL("kernel32.dll")
	u32                       = syscall.NewLazyDLL("user32.dll")
	pAttachConsole            = k32.NewProc("AttachConsole")
	pFreeConsole              = k32.NewProc("FreeConsole")
	pGetConsoleWindow         = k32.NewProc("GetConsoleWindow")
	pReadConsoleOutputCharW   = k32.NewProc("ReadConsoleOutputCharacterW")
	pGetConsoleScreenBufInf   = k32.NewProc("GetConsoleScreenBufferInfo")
	pWriteConsoleInputW       = k32.NewProc("WriteConsoleInputW")
	pSetConsoleCtrlHandler    = k32.NewProc("SetConsoleCtrlHandler")
	pCreateFileW              = k32.NewProc("CreateFileW")
	pProcessIdToSessionId     = k32.NewProc("ProcessIdToSessionId")
	pWTSGetActiveConsoleSessn = k32.NewProc("WTSGetActiveConsoleSessionId")
	pShowWindow               = u32.NewProc("ShowWindow")
	pSetForegroundWindow      = u32.NewProc("SetForegroundWindow")
)

const (
	genericRead   = 0x80000000
	genericWrite  = 0x40000000
	fileShare     = 0x3 // FILE_SHARE_READ | FILE_SHARE_WRITE
	openExisting  = 3
	invalidHandle = ^uintptr(0)
	keyEventType  = 0x0001
	vkReturn      = 0x0D
	swShowNormal  = 1
)

type coord struct{ X, Y int16 }
type smallRect struct{ Left, Top, Right, Bottom int16 }
type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// keyEventRecord is INPUT_RECORD with the KEY_EVENT_RECORD payload
// (EventType padded to 4 bytes; total 20 bytes, matching the C union).
type keyEventRecord struct {
	EventType uint16
	_         uint16
	KeyDown   int32
	RepeatCnt uint16
	VkCode    uint16
	VsCode    uint16
	UChar     uint16
	CtrlState uint32
}

// RunConOp implements the `hive __conop` helper. Two shapes:
//
//	__conop serve <pid> <creation>          persistent: attach once, then
//	                                         service framed ops on stdin
//	                                         until stdin closes (the fast
//	                                         path — see console_windows.go)
//	__conop <op> <pid> <creation> [arg]      one-shot: attach, run one op,
//	                                         exit (cold-start / fallback)
//
// One-shot results go to stdout (the whole payload); errors to stderr via
// the non-zero exit. The daemon-side wrappers are conOp / startHelper.
func RunConOp(args []string) error {
	if len(args) >= 1 && args[0] == "serve" {
		if len(args) < 3 {
			return fmt.Errorf("usage: __conop serve <pid> <creation>")
		}
		pid, err := strconv.ParseUint(args[1], 10, 32)
		if err != nil || pid == 0 {
			return fmt.Errorf("bad pid %q", args[1])
		}
		creation, err := strconv.ParseUint(args[2], 10, 64)
		if err != nil {
			return fmt.Errorf("bad creation %q", args[2])
		}
		return runConOpServe(uint32(pid), creation)
	}

	if len(args) < 3 {
		return fmt.Errorf("usage: __conop <op> <pid> <creation> [arg]")
	}
	op := args[0]
	pid, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil || pid == 0 {
		return fmt.Errorf("bad pid %q", args[1])
	}
	creation, err := strconv.ParseUint(args[2], 10, 64)
	if err != nil {
		return fmt.Errorf("bad creation %q", args[2])
	}
	arg := ""
	if len(args) > 3 {
		arg = args[3]
	}

	pinned, err := attachTo(uint32(pid), creation)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(pinned)
	defer pFreeConsole.Call()

	data, opErr := serveOne(op, arg)
	if opErr != nil {
		return opErr
	}
	if op == "capture" {
		_, err := os.Stdout.WriteString(data)
		return err
	}
	return nil
}

// attachTo pins + verifies the target (defeating pid reuse) and attaches
// this helper process to its console. The returned handle keeps the pid
// pinned; the caller MUST close it and FreeConsole. SYNCHRONIZE is
// requested so a persistent helper can WaitForSingleObject the target and
// self-exit when it dies.
func attachTo(pid uint32, creation uint64) (syscall.Handle, error) {
	h, err := openVerified(pid, creation, creation != 0, processQueryLimited|syscall.SYNCHRONIZE)
	if err != nil {
		return 0, err
	}
	// A CTRL_C_EVENT raised in the target console while attached (e.g. an
	// agent literally typing \x03 through hive keys) must not kill the
	// helper mid-op.
	pSetConsoleCtrlHandler.Call(0, 1)
	pFreeConsole.Call() // DETACHED_PROCESS has none; belt and braces
	if r, _, e := pAttachConsole.Call(uintptr(pid)); r == 0 {
		syscall.CloseHandle(h)
		return 0, fmt.Errorf("AttachConsole(%d): %v", pid, e)
	}
	return h, nil
}

// serveOne runs one console op against the currently-attached console and
// returns its payload/error. The caller decides what a failure means for
// helper lifetime (see runConOpServe).
func serveOne(op, arg string) (data string, opErr error) {
	switch op {
	case "capture":
		lines, _ := strconv.Atoi(arg)
		return opCapture(lines)
	case "keys":
		text, err := base64.StdEncoding.DecodeString(arg)
		if err != nil {
			return "", fmt.Errorf("bad keys payload: %v", err)
		}
		return "", opKeys(string(text))
	case "show":
		return "", opShow()
	}
	return "", fmt.Errorf("unknown console op %q", op)
}

// runConOpServe is the persistent helper: attach once, then loop reading
// tab-framed "op<TAB>arg\n" requests on stdin and writing "OK|ERR
// <base64 payload>\n" responses on stdout. Console I/O uses explicit
// CONIN$/CONOUT$ handles, so the stdio pipes stay free for framing (the
// same reason the one-shot writes results to os.Stdout after attaching).
func runConOpServe(pid uint32, creation uint64) error {
	pinned, err := attachTo(pid, creation)
	if err != nil {
		return err // exits non-zero; the daemon sees EOF on its first request and falls back
	}
	defer syscall.CloseHandle(pinned)
	defer pFreeConsole.Call()

	// Read stdin in a goroutine so the main loop can also poll the target's
	// liveness: an agent that exits on its own (not via `hive kill`) sends
	// no further ops and no stdin EOF, so without this the helper — and the
	// pinned handle that keeps the dead pid un-recyclable — would leak for
	// the daemon's lifetime.
	lines := make(chan string)
	go func() {
		rd := bufio.NewReader(os.Stdin)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- strings.TrimRight(line, "\r\n")
		}
	}()
	w := bufio.NewWriter(os.Stdout)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return nil // stdin closed: the daemon tore us down
			}
			op, arg, _ := strings.Cut(line, "\t")
			data, opErr := serveOne(op, arg)
			// keys/capture failures mean the console is suspect: reply, then
			// die so the daemon respawns a clean helper (show is benign).
			fatal := opErr != nil && (op == "keys" || op == "capture")
			if opErr != nil {
				writeFrame(w, "ERR", opErr.Error())
			} else {
				writeFrame(w, "OK", data)
			}
			w.Flush()
			if fatal {
				return nil
			}
		case <-tick.C:
			if ev, werr := syscall.WaitForSingleObject(pinned, 0); werr == nil && ev != waitTimeout {
				return nil // target exited: release the pinned handle and go
			}
		}
	}
}

func writeFrame(w *bufio.Writer, status, payload string) {
	w.WriteString(status)
	w.WriteByte(' ')
	w.WriteString(base64.StdEncoding.EncodeToString([]byte(payload)))
	w.WriteByte('\n')
}

func openConsole(name string) (uintptr, error) {
	namep, _ := syscall.UTF16PtrFromString(name)
	h, _, e := pCreateFileW.Call(
		uintptr(unsafe.Pointer(namep)),
		genericRead|genericWrite,
		fileShare, 0, openExisting, 0, 0)
	if h == invalidHandle {
		return 0, fmt.Errorf("open %s: %v", name, e)
	}
	return h, nil
}

// opCapture returns the console's visible window; lines > 0 extends the
// top upward into the scrollback buffer.
func opCapture(lines int) (string, error) {
	out, err := openConsole("CONOUT$")
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(syscall.Handle(out))
	var info consoleScreenBufferInfo
	if r, _, e := pGetConsoleScreenBufInf.Call(out, uintptr(unsafe.Pointer(&info))); r == 0 {
		return "", fmt.Errorf("GetConsoleScreenBufferInfo: %v", e)
	}
	top := int(info.Window.Top) - lines
	if top < 0 {
		top = 0
	}
	// ReadConsoleOutputCharacterW wraps at the BUFFER width (Size.X), not
	// the visible window width, so read full buffer rows in one call and
	// slice the visible columns [Left,Right] out of each — otherwise a
	// buffer wider than the window (or a horizontally scrolled window)
	// would misalign every row. One kernel call replaces the old
	// per-row loop (~50 rows on a full window, up to ~2000 on deep
	// scrollback). A child scrolling mid-read can still tear the snapshot,
	// accepted (unlike tmux's atomic capture).
	stride := int(info.Size.X)
	if stride <= 0 || stride > 8192 {
		return "", fmt.Errorf("bad console buffer width %d", stride)
	}
	left := int(info.Window.Left)
	right := int(info.Window.Right)
	if left < 0 {
		left = 0
	}
	if right >= stride {
		right = stride - 1
	}
	nrows := int(info.Window.Bottom) - top + 1
	if nrows <= 0 || right < left {
		return "", nil
	}
	total := stride * nrows
	buf := make([]uint16, total)
	var read uint32
	start := coord{X: 0, Y: int16(top)}
	if r, _, e := pReadConsoleOutputCharW.Call(
		out,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(total),
		uintptr(*(*uint32)(unsafe.Pointer(&start))),
		uintptr(unsafe.Pointer(&read))); r == 0 {
		return "", fmt.Errorf("ReadConsoleOutputCharacter: %v", e)
	}
	got := int(read)
	var b strings.Builder
	for row := 0; row < nrows; row++ {
		lo := row*stride + left
		hi := row*stride + right + 1
		if lo >= got {
			b.WriteByte('\n')
			continue
		}
		if hi > got {
			hi = got
		}
		b.WriteString(strings.TrimRight(syscall.UTF16ToString(buf[lo:hi]), " "))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// opKeys types text into the console input queue as key down/up pairs.
// '\r' carries VK_RETURN so line-oriented readers see a real Enter;
// characters outside the BMP are written as their UTF-16 surrogate
// halves (one record per code unit), which conhost reassembles.
func opKeys(text string) error {
	in, err := openConsole("CONIN$")
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(syscall.Handle(in))
	var recs []keyEventRecord
	for _, r := range text {
		var vk uint16
		if r == '\r' {
			vk = vkReturn
		}
		for _, u := range utf16.Encode([]rune{r}) {
			for _, down := range []int32{1, 0} {
				recs = append(recs, keyEventRecord{
					EventType: keyEventType, KeyDown: down,
					RepeatCnt: 1, VkCode: vk, UChar: u,
				})
			}
		}
	}
	// WriteConsoleInput writes only what fits in the input buffer and
	// never blocks; a large paste can outrun a child that is not reading
	// yet. Loop on the written count, backing off briefly when the
	// buffer is momentarily full, so the tail is not silently dropped.
	stalls := 0
	for len(recs) > 0 {
		n := len(recs)
		if n > 512 {
			n = 512
		}
		var written uint32
		r, _, e := pWriteConsoleInputW.Call(
			in,
			uintptr(unsafe.Pointer(&recs[0])),
			uintptr(n),
			uintptr(unsafe.Pointer(&written)))
		if r == 0 {
			return fmt.Errorf("WriteConsoleInput: %v", e)
		}
		if written == 0 {
			if stalls++; stalls > 200 { // ~2s of a wholly unread buffer
				return fmt.Errorf("WriteConsoleInput stalled (console input buffer full)")
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		stalls = 0
		recs = recs[written:]
	}
	return nil
}

// opShow reveals the console window — only sessions spawned headed have
// one (CREATE_NO_WINDOW consoles report a NULL HWND). A window on a
// non-interactive session (Session 0 under a --persist SYSTEM daemon)
// can never appear on the user's desktop, so that is reported as a
// failure rather than a false "opened".
func opShow() error {
	if err := interactiveSession(); err != nil {
		return err
	}
	hwnd, _, _ := pGetConsoleWindow.Call()
	if hwnd == 0 {
		return fmt.Errorf("session has no console window (spawned headless; respawn with --headed)")
	}
	pShowWindow.Call(hwnd, swShowNormal)
	pSetForegroundWindow.Call(hwnd)
	return nil
}

// interactiveSession reports whether this process runs in the session
// currently attached to the physical console (the user's desktop). A
// window shown from any other session — notably Session 0, where a
// SYSTEM service lives — is invisible to the user.
func interactiveSession() error {
	var mySession uint32
	r, _, _ := pProcessIdToSessionId.Call(uintptr(syscall.Getpid()), uintptr(unsafe.Pointer(&mySession)))
	if r == 0 {
		return fmt.Errorf("cannot determine the daemon's session")
	}
	active, _, _ := pWTSGetActiveConsoleSessn.Call()
	if mySession == 0 || uint32(active) != mySession {
		return fmt.Errorf("daemon is not in an interactive desktop session (session %d); --headed needs the daemon in the user's session, not a --persist SYSTEM service", mySession)
	}
	return nil
}
