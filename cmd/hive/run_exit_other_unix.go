//go:build aix || solaris

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// AIX and Solaris (including illumos, whose Go build also satisfies the
// solaris tag) use a conservative fallback. Their terminal ioctl interfaces
// differ from the common Unix implementation, so Hive leaves process-group
// and foreground-terminal ownership unchanged and forwards signals directly.
const runUsesProcessGroup = false

func runChildExitCode(err *exec.ExitError) int {
	if status, ok := err.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	if code := err.ExitCode(); code > 0 {
		return code
	}
	return 1
}

func prepareRunProcess(_ *exec.Cmd, _ *os.File) func() { return func() {} }

func signalRunProcess(root *os.Process, sig os.Signal) {
	_ = root.Signal(sig)
}
