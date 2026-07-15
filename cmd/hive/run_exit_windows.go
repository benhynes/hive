//go:build windows

package main

import (
	"os"
	"os/exec"
)

const runUsesProcessGroup = false

// Windows process exit codes do not use the Unix 128+signal convention.
func runChildExitCode(err *exec.ExitError) int {
	if code := err.ExitCode(); code > 0 {
		return code
	}
	return 1
}

func prepareRunProcess(_ *exec.Cmd, _ *os.File) func() { return func() {} }

func signalRunProcess(root *os.Process, sig os.Signal) {
	_ = root.Signal(sig)
}
