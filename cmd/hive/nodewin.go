package main

// Windows targets for `hive node install`. Windows OpenSSH has no POSIX
// shell, so every remote action runs as a PowerShell script passed via
// -EncodedCommand (base64 UTF-16LE) — immune to the cmd/powershell
// default-shell quoting maze — and files travel over scp/sftp instead
// of stdin pipes. Control ops (spawn/read/keys/kill) work through the
// Windows console backend (internal/control); pass --msg-only to
// withhold the control token, or --local-control to mint one that is valid
// only on the Windows node.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/proto"
	"github.com/benhynes/hive/internal/sshx"
)

type winOpts struct {
	name, bind, hub, dest, home, bin, src string
	port                                  int
	controlMode                           nodeControlMode
	controlToken                          string
	persist, noStart, restart, noAnnounce bool
	computer, goarch, profile             string
}

// parseWinProbe splits `echo %OS% %COMPUTERNAME% %PROCESSOR_ARCHITECTURE%
// %USERPROFILE%` output. The profile is last because it may contain
// spaces.
func parseWinProbe(out string) (computer, goarch, profile string, err error) {
	parts := strings.SplitN(strings.TrimSpace(out), " ", 4)
	if len(parts) < 4 || parts[0] != "Windows_NT" {
		return "", "", "", fmt.Errorf("unexpected probe output %q", strings.TrimSpace(out))
	}
	switch strings.ToUpper(parts[2]) {
	case "AMD64":
		goarch = "amd64"
	case "ARM64":
		goarch = "arm64"
	default:
		return "", "", "", fmt.Errorf("unsupported Windows arch %q", parts[2])
	}
	return parts[1], goarch, strings.TrimSpace(parts[3]), nil
}

// winPS wraps a PowerShell script for the remote command line. Encoded
// commands survive any default shell unchanged.
func winPS(script string) string {
	u16 := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u16))
	for i, r := range u16 {
		b[2*i], b[2*i+1] = byte(r), byte(r>>8)
	}
	return "powershell -NoProfile -NonInteractive -EncodedCommand " + base64.StdEncoding.EncodeToString(b)
}

// winPathRe: absolute drive paths, no quotes and no spaces (spaces would
// need per-context quoting in scheduled tasks and Start-Process).
var winPathRe = regexp.MustCompile(`^[A-Za-z]:\\[A-Za-z0-9_.\\-]+$`)

func winPath(p, what string) (string, error) {
	p = strings.TrimRight(p, `\`)
	if !winPathRe.MatchString(p) {
		return "", fmt.Errorf("%s %q must be an absolute Windows path without spaces or quotes", what, p)
	}
	return p, nil
}

func nodeInstallWindows(ssh sshx.Runner, cfg config.Config, netName string, nc config.NetConfig, o winOpts) error {
	if o.name == "" {
		o.name = config.Sanitize(strings.ToLower(o.computer))
	}
	if !proto.ValidName(o.name) {
		return fmt.Errorf("bad node name %q", o.name)
	}
	if o.name == cfg.HostName {
		return fmt.Errorf("node would be named %q, same as this host — pass --name", o.name)
	}
	layer := "control via the Windows console backend"
	if o.controlMode == nodeControlNone {
		layer = "message-only"
	} else if o.controlMode == nodeControlLocal {
		layer = "host-local control (--local-control)"
	}
	fmt.Printf("  windows/%s, node name %q — %s\n", o.goarch, o.name, layer)

	profile, err := winPath(o.profile, "%USERPROFILE%")
	if err != nil {
		return fmt.Errorf("%v — pass --dest and --home explicitly", err)
	}
	if o.dest == "" {
		o.dest = profile + `\.hive\bin\hive.exe`
	}
	if o.home == "" {
		o.home = profile + `\.hive`
	}
	if o.dest, err = winPath(o.dest, "--dest"); err != nil {
		return err
	}
	if o.home, err = winPath(o.home, "--home"); err != nil {
		return err
	}
	q := func(s string) string { return "'" + s + "'" } // winPath forbids quotes

	// Admin decides firewall + --persist capability.
	adminOut, _ := ssh.Run(nil, winPS(`[bool](New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)`))
	admin := strings.Contains(adminOut, "True")

	// Binary for windows/<arch>.
	binPath := o.bin
	if binPath == "" {
		if binPath, err = crossCompile("windows", o.goarch, o.src); err != nil {
			return err
		}
	}

	// Directories, then the binary over scp.
	destDir := o.dest[:strings.LastIndex(o.dest, `\`)]
	netDir := o.home + `\nets\` + netName
	if _, err := ssh.Run(nil, winPS(fmt.Sprintf(
		`New-Item -ItemType Directory -Force -Path %s,%s,%s | Out-Null`,
		q(destDir), q(o.home), q(netDir)))); err != nil {
		return err
	}
	fmt.Printf("installing %s -> %s:%s ...\n", binPath, ssh.Target, o.dest)
	if err := ssh.SCP(binPath, o.dest+".tmp"); err != nil {
		return err
	}
	if o.restart || o.persist {
		ssh.Run(nil, winPS(`Stop-Process -Name hive -Force -ErrorAction SilentlyContinue; Start-Sleep -Milliseconds 300`))
	}
	if _, err := ssh.Run(nil, winPS(fmt.Sprintf(`Move-Item -Force %s %s`, q(o.dest+".tmp"), q(o.dest)))); err != nil {
		return fmt.Errorf("%v (a running daemon locks the binary — pass --restart to upgrade)", err)
	}

	// Addresses.
	if o.bind == "" {
		if o.bind = winTailnetIP(ssh); o.bind == "" {
			return fmt.Errorf("cannot detect the node's tailscale IPv4 — pass --bind ADDR")
		}
	}
	if net.ParseIP(o.bind) == nil {
		return fmt.Errorf("bad --bind %q (want an IP address)", o.bind)
	}
	hub, err := resolveHubAddr(cfg, o.hub)
	if err != nil {
		return err
	}
	warnLoopbackHub(cfg, hub)

	// State files travel over scp — tokens never touch remote argv.
	fmt.Printf("configuring: host_name=%s bind=%s port=%d net=%s control=%s admin=%v\n",
		o.name, o.bind, o.port, netName, o.controlMode, admin)
	tmp, err := os.MkdirTemp("", "hive-win")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	nodeCfg, _ := json.MarshalIndent(config.Config{HostName: o.name, Bind: o.bind, Port: o.port}, "", "  ")
	nodeNet := config.NetConfig{
		Name:     netName,
		MsgToken: nc.MsgToken,
		Hosts:    seedHosts(nc.Hosts, cfg.HostName, hub, o.name, o.port),
	}
	nodeNet.ControlToken = o.controlToken
	if o.controlMode == nodeControlLocal {
		nodeNet.ControlHost = o.name
	}
	nodeNetJSON, _ := json.MarshalIndent(nodeNet, "", "  ")
	for _, f := range []struct {
		name   string
		data   []byte
		remote string
	}{
		{"config.json", nodeCfg, o.home + `\config.json`},
		{"net.json", nodeNetJSON, netDir + `\net.json`},
	} {
		local := filepath.Join(tmp, f.name)
		if err := os.WriteFile(local, f.data, 0o600); err != nil {
			return err
		}
		if err := ssh.SCP(local, f.remote); err != nil {
			return err
		}
	}

	// Windows Defender blocks inbound by default; open the daemon port.
	if admin {
		if _, err := ssh.Run(nil, winPS(fmt.Sprintf(
			`if (-not (Get-NetFirewallRule -DisplayName 'hive' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -DisplayName 'hive' -Direction Inbound -Action Allow -Protocol TCP -LocalPort %d | Out-Null }`,
			o.port))); err != nil {
			fmt.Printf("WARNING: firewall rule failed (%v) — inbound %d may be blocked\n", err, o.port)
		} else {
			fmt.Printf("firewall: inbound TCP %d allowed (rule 'hive')\n", o.port)
		}
	} else {
		fmt.Printf("WARNING: ssh user is not admin — cannot open the firewall; inbound %d may be blocked\n", o.port)
	}

	// Start (or persist) the daemon.
	nodeURL := "http://" + net.JoinHostPort(o.bind, strconv.Itoa(o.port))
	hint := fmt.Sprintf("ssh %s type %s\\daemon.log", ssh.Target, o.home)
	if o.persist {
		if !admin {
			return fmt.Errorf("--persist on Windows needs an admin ssh user (creates a boot scheduled task)")
		}
		// SYSTEM's own profile is not ours, so the task pins the state
		// dir explicitly via `daemon --home`.
		create := fmt.Sprintf(
			`schtasks /Create /F /TN hive /SC ONSTART /RU SYSTEM /TR '"%s" daemon --home "%s"'; if ($LASTEXITCODE -ne 0) { exit 1 }`,
			o.dest, o.home)
		if _, err := ssh.Run(nil, winPS(create)); err != nil {
			return fmt.Errorf("schtasks create: %v", err)
		}
		fmt.Println("persistence: scheduled task 'hive' (runs at boot as SYSTEM; boot-start only — schtasks does not restart a crashed daemon)")
		if !o.noStart {
			if _, err := ssh.Run(nil, winPS(`schtasks /Run /TN hive | Out-Null; if ($LASTEXITCODE -ne 0) { exit 1 }`)); err != nil {
				return fmt.Errorf("schtasks run: %v", err)
			}
			if err := waitHealthy(nodeURL, hint); err != nil {
				return err
			}
		}
	} else if !o.noStart {
		running := healthOK(nodeURL)
		if running && !o.restart {
			fmt.Println("daemon already running — the old binary stays in memory (pass --restart to upgrade)")
		} else {
			// --restart already stopped it above (before the binary swap).
			//
			// Launch detached via WMI, not Start-Process: Windows OpenSSH
			// terminates the session's whole process tree on disconnect,
			// so a hidden Start-Process child dies the moment this install
			// closes its ssh connection. Win32_Process.Create runs the
			// process from the WMI service, outside the ssh session's job,
			// so a non-persist daemon outlives the install. cmd wraps it
			// for stdout/stderr redirection; the winPath-validated paths
			// have no spaces or quotes, so they pass unquoted and avoid
			// the `cmd /c` leading-quote rule.
			cmdLine := fmt.Sprintf(`cmd /c %s daemon --home %s > %s 2> %s`,
				o.dest, o.home, o.home+`\daemon.log`, o.home+`\daemon.err`)
			start := fmt.Sprintf(
				`try { $r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=%s} -ErrorAction Stop } catch { Write-Output $_.Exception.Message; exit 1 }; if ($r.ReturnValue -ne 0) { Write-Output ('Win32_Process.Create returned ' + $r.ReturnValue); exit 1 }`,
				q(cmdLine))
			if _, err := ssh.Run(nil, winPS(start)); err != nil {
				return err
			}
			if err := waitHealthy(nodeURL, hint); err != nil {
				return err
			}
		}
	}

	nodeAddr := net.JoinHostPort(o.bind, strconv.Itoa(o.port))
	if err := announceAll(cfg, netName, nc, o.name, nodeAddr, o.noAnnounce); err != nil {
		return err
	}

	fmt.Printf("\nnode %q is in the mesh:\n", o.name)
	fmt.Printf("  hive agents                        # should reach @%s\n", o.name)
	if o.controlMode == nodeControlShared {
		fmt.Printf("  hive spawn --host %s <name> -- CMD...\n", o.name)
	} else if o.controlMode == nodeControlLocal {
		fmt.Printf("  on %s: hive spawn --grant-control <name> -- CMD...\n", o.name)
		fmt.Printf("note: %s has host-local control; the original network control token cannot control it remotely\n", o.name)
	}
	if o.noStart {
		// A plain `ssh <host> hive daemon` runs in the foreground and dies
		// when that ssh session closes (OpenSSH kills the session tree);
		// launch it detached, or rerun install without --no-start.
		fmt.Printf("  start it (detached): ssh %s powershell -Command \"Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine='cmd /c %s daemon --home %s'}\"\n",
			ssh.Target, o.dest, o.home)
	}
	if !o.persist {
		fmt.Printf("note: the daemon is not persisted across reboots (rerun with --persist)\n")
	}
	return nil
}

// winTailnetIP finds the node's tailscale IPv4: the CLI if on PATH,
// else the first interface address in CGNAT 100.64.0.0/10.
func winTailnetIP(ssh sshx.Runner) string {
	if out, err := ssh.Run(nil, `tailscale ip -4`); err == nil {
		ip := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
		if p := net.ParseIP(ip); p != nil && isCGNAT(p) {
			return ip
		}
	}
	out, err := ssh.Run(nil, winPS(`(Get-NetIPAddress -AddressFamily IPv4).IPAddress`))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if p := net.ParseIP(strings.TrimSpace(line)); p != nil && isCGNAT(p) {
			return p.String()
		}
	}
	return ""
}
