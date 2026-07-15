package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/benhynes/hive/internal/client"
	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/proto"
)

// runExitError carries an attached child's status through main without
// turning it into a generic "hive: ..." failure. A launcher should behave
// like the command it wraps in pipelines and scripts.
type runExitError struct {
	code int
}

func (e *runExitError) Error() string { return fmt.Sprintf("child exited with status %d", e.code) }
func (e *runExitError) ExitCode() int { return e.code }

// runRun launches a command attached to the caller's terminal while giving it
// a leased, agent-scoped Hive identity. No CONTROL credential is injected and
// tmux is deliberately not involved: the child keeps the caller's normal
// stdin/stdout/stderr and ordinary exit/interrupt semantics. Interactive shell
// job control is not proxied; in particular, Ctrl-Z is unsupported.
func runRun(args []string) error {
	fs := flags("run", args)
	name := fs.String("name", "", "agent name (default: command name plus a unique suffix)")
	cwd := fs.String("cwd", "", "working directory for the child process")
	fs.Parse2()
	argv := fs.afterDD
	if len(argv) == 0 {
		return fmt.Errorf("usage: hive run [--name N] [--cwd D] -- CMD...")
	}
	generatedName := *name == ""
	c, err := client.ResolveBootstrap(*fs.net)
	if err != nil {
		return err
	}
	var reg client.RegisterResp
	if generatedName {
		for attempt := 0; attempt < 4; attempt++ {
			*name = automaticRunName(argv[0])
			reg, err = c.RegisterEphemeralLease(*name, client.DefaultLeaseSeconds)
			if err == nil || !strings.Contains(err.Error(), "taken by a live agent") {
				break
			}
		}
	} else {
		reg, err = c.RegisterLease(*name, "", 0, client.DefaultLeaseSeconds)
	}
	if err != nil {
		return fmt.Errorf("register %q: %w", *name, err)
	}

	// From here on this client is the new agent, not the operator who launched
	// it. Clearing CONTROL makes cleanup and heartbeats least-privileged too.
	c.Agent = reg.Agent
	c.Token = reg.Token
	c.Control = ""
	c.ControlHost = ""
	leaseCtx, cancelLease := context.WithCancel(context.Background())
	c.SetContext(leaseCtx)

	var heartbeatStopped chan struct{}
	defer func() {
		cancelLease()
		if heartbeatStopped != nil {
			// Do not race a final renewal against deregistration. Cancelling the
			// request context also interrupts a heartbeat already in flight.
			<-heartbeatStopped
		}
		// Lease expiry is the crash fallback, so cleanup failure must not
		// replace the attached command's exit status or hold up the terminal.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		c.SetContext(cleanupCtx)
		var err error
		if generatedName {
			err = c.Deregister("")
		} else {
			_, err = c.ReleaseLease()
		}
		cleanupCancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "hive run: release %s: %v (lease will expire)\n", reg.Agent, err)
		}
	}()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = *cwd
	cmd.Env = runEnvironment(os.Environ(), c, reg)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	restoreTerminal := prepareRunProcess(cmd, os.Stdin)
	restoredTerminal := false
	defer func() {
		if !restoredTerminal {
			restoreTerminal()
		}
	}()

	// Install handlers before Start so a direct SIGINT/SIGTERM cannot land in
	// the small window where a child exists but forwarding is not active. A
	// buffered signal waits until cmd.Process is available below.
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	signalDone := make(chan struct{})
	defer func() {
		signal.Stop(sigc)
		close(signalDone)
	}()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %q: %w", argv[0], err)
	}
	go forwardRunSignals(cmd.Process, sigc, signalDone)

	fmt.Fprintf(os.Stderr, "hive run: %s (agent-scoped, leased; no control injected)\n", reg.Agent)
	heartbeatStopped = make(chan struct{})
	go func() {
		defer close(heartbeatStopped)
		heartbeatUntil(leaseCtx, c, time.Duration(client.DefaultHeartbeatSeconds)*time.Second, os.Stderr)
	}()

	// Catch direct SIGINT/SIGTERM delivery that would otherwise terminate Hive
	// before its defers run, and forward it to the child. On common Unix the
	// child owns the terminal's foreground process group, so terminal-generated
	// signals reach it without also reaching this wrapper. Other platforms use
	// best-effort direct forwarding.
	waitErr := cmd.Wait()
	restoreTerminal()
	restoredTerminal = true
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return &runExitError{code: runChildExitCode(exitErr)}
		}
		return fmt.Errorf("wait for %q: %w", argv[0], waitErr)
	}
	return nil
}

// automaticRunName keeps the runtime recognizable in agent listings while a
// random suffix makes concurrent invocations effectively collision-free.
func automaticRunName(command string) string {
	prefix := strings.TrimLeft(config.Sanitize(filepath.Base(command)), "-_")
	if prefix == "" {
		prefix = "agent"
	}
	const suffixLen = 16
	const maxPrefix = 32 - 1 - suffixLen
	if len(prefix) > maxPrefix {
		prefix = prefix[:maxPrefix]
	}
	return prefix + "-" + proto.NewToken()[:suffixLen]
}

// runEnvironment replaces any enclosing agent identity and removes CONTROL
// capabilities. Setting a personal HIVE_TOKEN also prevents config fallback.
// This is capability selection for cooperative same-user processes, not an OS
// sandbox: a child that can read the operator's HIVE_HOME can read its files.
func runEnvironment(base []string, c *client.Client, reg client.RegisterResp) []string {
	remove := []string{
		"HIVE_ADDR", "HIVE_NET", "HIVE_AGENT", "HIVE_TOKEN",
		"HIVE_CONTROL_TOKEN", "HIVE_CONTROL_HOST",
	}
	out := make([]string, 0, len(base)+4)
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || containsEnvKey(remove, key) {
			continue
		}
		out = append(out, entry)
	}
	return append(out,
		"HIVE_ADDR="+c.Addr,
		"HIVE_NET="+c.Net,
		"HIVE_AGENT="+reg.Agent,
		"HIVE_TOKEN="+reg.Token,
	)
}

func containsEnvKey(keys []string, candidate string) bool {
	for _, key := range keys {
		if key == candidate || (runtime.GOOS == "windows" && strings.EqualFold(key, candidate)) {
			return true
		}
	}
	return false
}

type runHeartbeater interface {
	HeartbeatContext(context.Context) (client.HeartbeatResp, error)
}

func heartbeatUntil(ctx context.Context, c runHeartbeater, interval time.Duration, errOut io.Writer) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	retry := interval / 3
	if retry <= 0 {
		retry = interval
	}
	failed := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			beatCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := c.HeartbeatContext(beatCtx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				if !failed {
					fmt.Fprintf(errOut, "hive run: heartbeat: %v\n", err)
				}
				failed = true
				timer.Reset(retry)
			} else {
				failed = false
				timer.Reset(interval)
			}
		}
	}
}

func forwardRunSignals(p *os.Process, signals <-chan os.Signal, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case sig := <-signals:
			if sig != nil {
				signalRunProcess(p, sig)
			}
		}
	}
}
