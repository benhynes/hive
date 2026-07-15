// Package sshx is hive's SSH transport: a ControlMaster-multiplexed runner
// for remote commands, file shipping, safe remote writes, platform probing,
// and (for SSH hosts) live port-forward management on the master connection.
//
// Promoted out of cmd/hive/node.go so both the CLI (`hive node install`) and
// the hub (SSH-host bring-up) share one implementation.
package sshx

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// bin returns the ssh-family binary to exec. HIVE_SSH_BIN overrides it —
// a test hook so e2e tests can substitute a local-exec shim; never set it
// in production.
func bin(name string) string {
	if b := os.Getenv("HIVE_SSH_BIN"); b != "" && name == "ssh" {
		return b
	}
	if b := os.Getenv("HIVE_SCP_BIN"); b != "" && name == "scp" {
		return b
	}
	return name
}

// Runner executes commands on one SSH target over a shared ControlMaster
// connection.
type Runner struct {
	Target   string
	Identity string // optional ssh key path (-i)
	ctlPath  string
}

// NewRunner builds a runner backed by an SSH ControlMaster so every ssh/scp
// invocation reuses one authenticated connection. The cleanup closes the
// master (ssh -O exit) and removes the socket dir; call it via defer. If a
// temp dir can't be made it degrades to unmultiplexed ssh.
func NewRunner(target string) (Runner, func()) {
	dir, err := os.MkdirTemp("/tmp", "hive-cm")
	if err != nil {
		return Runner{Target: target}, func() {}
	}
	ctl := dir + "/s" // short: the ControlPath socket must fit sun_path (~104)
	r := Runner{Target: target, ctlPath: ctl}
	cleanup := func() {
		exec.Command(bin("ssh"), append(r.opts(), "-O", "exit", target)...).Run()
		os.RemoveAll(dir)
	}
	return r, cleanup
}

func (s Runner) opts() []string {
	o := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	if s.Identity != "" {
		o = append(o, "-i", s.Identity)
	}
	if s.ctlPath != "" {
		o = append(o, "-o", "ControlMaster=auto", "-o", "ControlPath="+s.ctlPath, "-o", "ControlPersist=30s")
	}
	return o
}

// Run executes cmd on the target, feeding stdin when non-nil.
func (s Runner) Run(stdin io.Reader, cmd string) (string, error) {
	c := exec.Command(bin("ssh"), append(s.opts(), s.Target, cmd)...)
	if stdin != nil {
		c.Stdin = stdin
	}
	var out, errb strings.Builder
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("ssh %s: %v: %s", s.Target, err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// SCP copies a local file to a remote path over the sftp channel (works on
// Windows OpenSSH too, where there is no POSIX shell to pipe into).
func (s Runner) SCP(local, remote string) error {
	remote = strings.ReplaceAll(remote, `\`, `/`)
	c := exec.Command(bin("scp"), append(s.opts(), "-q", local, s.Target+":"+remote)...)
	var errb strings.Builder
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		return fmt.Errorf("scp to %s:%s: %v: %s", s.Target, remote, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// WriteRemote streams content into a remote file under dir, 0600, via the
// ssh channel — secrets never appear in remote argv.
func (s Runner) WriteRemote(dir, path string, content []byte) error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	go func() {
		w.Write(content)
		w.Close()
	}()
	defer r.Close()
	script := fmt.Sprintf(`sh -c 'set -e; umask 077; mkdir -p "%s"; cat > "%s"'`, dir, path)
	_, err = s.Run(r, script)
	return err
}

// ForwardL adds a local→remote forward (-L) on the live master connection:
// 127.0.0.1:localPort here reaches 127.0.0.1:remotePort there.
func (s Runner) ForwardL(localPort, remotePort int) error {
	spec := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", localPort, remotePort)
	_, err := s.ctl("-O", "forward", "-L", spec)
	return err
}

// ForwardR adds a remote→local reverse forward (-R) on the live master,
// letting the remote reach 127.0.0.1:localPort here. Passing remotePort 0
// asks the remote sshd to allocate one; the allocated port is returned.
func (s Runner) ForwardR(remotePort, localPort int) (int, error) {
	spec := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", remotePort, localPort)
	out, err := s.ctl("-O", "forward", "-R", spec)
	if err != nil {
		return 0, err
	}
	if remotePort != 0 {
		return remotePort, nil
	}
	// ssh prints the allocated port (bare number) on stdout.
	m := regexp.MustCompile(`\d+`).FindString(out)
	p, aerr := strconv.Atoi(m)
	if m == "" || aerr != nil || p == 0 {
		return 0, fmt.Errorf("remote forward: could not parse allocated port from %q", strings.TrimSpace(out))
	}
	return p, nil
}

// ctl issues a ControlMaster command (-O ...) against the live connection.
func (s Runner) ctl(args ...string) (string, error) {
	if s.ctlPath == "" {
		return "", fmt.Errorf("no ControlMaster socket — port forwards need a multiplexed connection")
	}
	c := exec.Command(bin("ssh"), append(append(s.opts(), args...), s.Target)...)
	var out, errb strings.Builder
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("ssh -O %s %s: %v: %s", args[1], s.Target, err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

var remotePathRe = regexp.MustCompile(`^[A-Za-z0-9_./$-]+$`)

// RemotePath normalizes a remote dir/file value for safe embedding in a
// double-quoted remote sh script. Leading ~/ becomes $HOME/.
func RemotePath(p string) (string, error) {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		p = "$HOME/" + rest
	}
	if !remotePathRe.MatchString(p) {
		return "", fmt.Errorf("remote path %q may only contain [A-Za-z0-9_./$-]", p)
	}
	return p, nil
}

// PlatformOf maps uname output to GOOS/GOARCH.
func PlatformOf(unameS, unameM string) (string, string, error) {
	var goos, goarch string
	switch strings.ToLower(unameS) {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "darwin"
	default:
		return "", "", fmt.Errorf("unsupported node OS %q", unameS)
	}
	switch strings.ToLower(unameM) {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", fmt.Errorf("unsupported node arch %q", unameM)
	}
	return goos, goarch, nil
}
