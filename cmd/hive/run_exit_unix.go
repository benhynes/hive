//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"unsafe"
)

const runUsesProcessGroup = true

// runChildExitCode preserves the shell convention for signal deaths. Go's
// ExitCode reports -1 in that case, but callers of an attached launcher
// expect SIGINT=130, SIGTERM=143, and so on.
func runChildExitCode(err *exec.ExitError) int {
	if status, ok := err.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	if code := err.ExitCode(); code > 0 {
		return code
	}
	return 1
}

// prepareRunProcess gives the command its own process group. When Hive itself
// owns the controlling terminal's foreground, SysProcAttr.Foreground transfers
// that terminal atomically before exec; the returned closure restores Hive's
// group after Wait. Forwarded signals then reach ordinary descendants that
// remain in that group, while avoiding the duplicate SIGINT that occurs when
// wrapper and child share a foreground group. Descendants that create a new
// process group are outside that forwarding boundary.
// It intentionally does not proxy interactive shell job control: Ctrl-Z stops
// the child group without suspending Hive or returning control to its parent.
func prepareRunProcess(cmd *exec.Cmd, tty *os.File) func() {
	attr := &syscall.SysProcAttr{Setpgid: true}
	parentPgrp := syscall.Getpgrp()
	fd := uintptr(0)
	foreground := false
	if tty != nil {
		fd = tty.Fd()
		if current, err := terminalPgrp(fd); err == nil && int(current) == parentPgrp {
			attr.Foreground = true
			attr.Ctty = int(fd)
			foreground = true
		}
	}
	cmd.SysProcAttr = attr
	return func() {
		if !foreground {
			return
		}
		// A background process that changes terminal foreground ownership is
		// normally stopped by SIGTTOU. Ignore it only around the restoring ioctl;
		// the child was already exec'd before this process-wide change.
		signal.Ignore(syscall.SIGTTOU)
		pgrp := int32(parentPgrp)
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSPGRP), uintptr(unsafe.Pointer(&pgrp)))
		signal.Reset(syscall.SIGTTOU)
	}
}

func terminalPgrp(fd uintptr) (int32, error) {
	var pgrp int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGPGRP), uintptr(unsafe.Pointer(&pgrp)))
	if errno != 0 {
		return 0, errno
	}
	return pgrp, nil
}

// signalRunProcess forwards one signal to the child's entire process group.
func signalRunProcess(root *os.Process, sig os.Signal) {
	if unixSignal, ok := sig.(syscall.Signal); ok {
		if err := syscall.Kill(-root.Pid, unixSignal); err == nil {
			return
		}
	}
	_ = root.Signal(sig)
}
