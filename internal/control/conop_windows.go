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

// RunConOp implements `hive __conop <op> <pid> <creation> [arg]`. Ops:
// capture <lines>, keys <base64 text>, show. Results go to stdout (the
// whole payload — the daemon reads all of it), errors to stderr via the
// non-zero exit; the daemon-side wrapper is conOp in console_windows.go.
func RunConOp(args []string) error {
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

	// Pin + verify the target before attaching (see file comment).
	h, err := openVerified(uint32(pid), creation, creation != 0, processQueryLimited)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)

	// A CTRL_C_EVENT raised in the target console while attached (e.g.
	// an agent literally typing \x03 through hive keys) must not kill
	// the helper mid-op.
	pSetConsoleCtrlHandler.Call(0, 1)
	pFreeConsole.Call() // DETACHED_PROCESS has none; belt and braces
	if r, _, e := pAttachConsole.Call(uintptr(pid)); r == 0 {
		return fmt.Errorf("AttachConsole(%d): %v", pid, e)
	}
	defer pFreeConsole.Call()

	switch op {
	case "capture":
		lines, _ := strconv.Atoi(arg)
		return opCapture(lines)
	case "keys":
		text, err := base64.StdEncoding.DecodeString(arg)
		if err != nil {
			return fmt.Errorf("bad keys payload: %v", err)
		}
		return opKeys(string(text))
	case "show":
		return opShow()
	}
	return fmt.Errorf("unknown console op %q", op)
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

// opCapture prints the console's visible window; lines > 0 extends the
// top upward into the scrollback buffer.
func opCapture(lines int) error {
	out, err := openConsole("CONOUT$")
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(syscall.Handle(out))
	var info consoleScreenBufferInfo
	if r, _, e := pGetConsoleScreenBufInf.Call(out, uintptr(unsafe.Pointer(&info))); r == 0 {
		return fmt.Errorf("GetConsoleScreenBufferInfo: %v", e)
	}
	top := int(info.Window.Top) - lines
	if top < 0 {
		top = 0
	}
	width := int(info.Window.Right-info.Window.Left) + 1
	if width <= 0 || width > 4096 {
		return fmt.Errorf("bad console window width %d", width)
	}
	// Rows are read one at a time (ReadConsoleOutputCharacterW takes a
	// single start coord); a child scrolling mid-read can tear the
	// snapshot — accepted, unlike tmux's atomic capture.
	var b strings.Builder
	buf := make([]uint16, width)
	for row := top; row <= int(info.Window.Bottom); row++ {
		var read uint32
		start := coord{X: info.Window.Left, Y: int16(row)}
		if r, _, e := pReadConsoleOutputCharW.Call(
			out,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(width),
			uintptr(*(*uint32)(unsafe.Pointer(&start))),
			uintptr(unsafe.Pointer(&read))); r == 0 {
			return fmt.Errorf("ReadConsoleOutputCharacter row %d: %v", row, e)
		}
		b.WriteString(strings.TrimRight(syscall.UTF16ToString(buf[:read]), " "))
		b.WriteByte('\n')
	}
	_, err = os.Stdout.WriteString(b.String())
	return err
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
