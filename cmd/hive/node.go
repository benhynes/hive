package main

// hive node install — bootstrap a new mesh host over ssh: ship the
// binary, write its config + network state (tokens travel over the ssh
// channel, never in remote argv), start the daemon, and announce the
// new host to every hub this one already knows.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/proto"
	"github.com/benhynes/hive/internal/sshx"
)

func runNode(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hive node install [flags] <ssh-target>")
	}
	switch args[0] {
	case "install":
		return nodeInstall(args[1:])
	default:
		return fmt.Errorf("unknown: hive node %s", args[0])
	}
}

type nodeControlMode uint8

const (
	nodeControlNone nodeControlMode = iota
	nodeControlShared
	nodeControlLocal
)

func (m nodeControlMode) String() string {
	switch m {
	case nodeControlShared:
		return "shared"
	case nodeControlLocal:
		return "host-local"
	default:
		return "msg-only"
	}
}

// selectNodeControl chooses the capability installed on a new node. A
// host-local source token is never inherited: callers must explicitly mint a
// different host-local token for the child with --local-control.
func selectNodeControl(nc config.NetConfig, msgOnly, localControl bool) (nodeControlMode, string, error) {
	if msgOnly && localControl {
		return nodeControlNone, "", fmt.Errorf("--msg-only and --local-control are mutually exclusive")
	}
	if msgOnly {
		return nodeControlNone, "", nil
	}
	if localControl {
		for {
			tok := proto.NewToken()
			if tok != nc.MsgToken && tok != nc.ControlToken {
				return nodeControlLocal, tok, nil
			}
		}
	}
	if nc.ControlToken == "" || nc.ControlHost != "" {
		return nodeControlNone, "", nil
	}
	return nodeControlShared, nc.ControlToken, nil
}

func nodeInstall(args []string) error {
	fs := flags("node install", args)
	name := fs.String("name", "", "node's hive host name (default: its hostname)")
	bind := fs.String("bind", "", "address the node's daemon binds (default: its tailscale IPv4)")
	port := fs.Int("port", 7777, "node's daemon port")
	hubAddr := fs.String("hub", "", "this hub's addr:port as reachable FROM the node (default: local tailscale IPv4 + local port)")
	dest := fs.String("dest", "", "remote path for the binary (default: ~/.local/bin/hive; %USERPROFILE%\\.hive\\bin\\hive.exe on Windows)")
	home := fs.String("home", "", "remote hive state dir (default: ~/.hive)")
	bin := fs.String("bin", "", "prebuilt binary to ship (default: self-copy, or cross-compile from --src)")
	src := fs.String("src", ".", "hive source dir for cross-compiling")
	msgOnly := fs.Bool("msg-only", false, "don't ship the control token (node can never control anyone)")
	localControl := fs.Bool("local-control", false, "mint control access valid only on the new node")
	persist := fs.Bool("persist", false, "install a supervisor: systemd (Linux), launchd (macOS), or a boot scheduled task (Windows)")
	noStart := fs.Bool("no-start", false, "install and configure only; don't start the daemon")
	restart := fs.Bool("restart", false, "restart the node's daemon if one is already running (upgrades)")
	noAnnounce := fs.Bool("no-announce", false, "don't add the node to the other hubs' hosts lists")
	fs.Parse2()
	target := fs.pos(0)
	if target == "" {
		return fmt.Errorf("usage: hive node install [flags] <ssh-target>   (flags before the target)")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	netName, nc, err := resolveNetConfig(*fs.net)
	if err != nil {
		return err
	}
	controlMode, nodeControlToken, err := selectNodeControl(nc, *msgOnly, *localControl)
	if err != nil {
		return err
	}
	if controlMode == nodeControlNone && !*msgOnly && nc.ControlHost != "" {
		fmt.Printf("note: this host's control token is scoped to %s — installing the node msg-only (pass --local-control for an independent local token)\n", nc.ControlHost)
	} else if controlMode == nodeControlNone && !*msgOnly {
		fmt.Println("note: this host holds no control token — installing the node msg-only")
	}
	// A local-control reinstall rotates the node's token, so a running daemon
	// must reload it. Treat --restart as implied unless --no-start suppresses
	// all daemon lifecycle actions.
	effectiveRestart := *restart || controlMode == nodeControlLocal

	ssh, sshCleanup := sshx.NewRunner(target)
	defer sshCleanup()

	// 1. What are we installing onto?
	fmt.Printf("probing %s ...\n", target)
	out, uerr := ssh.Run(nil, `uname -s -n -m`)
	if uerr != nil {
		// No POSIX shell — Windows OpenSSH (cmd or powershell default
		// shell) answers this probe instead.
		if wout, werr := ssh.Run(nil, `cmd /c "echo %OS% %COMPUTERNAME% %PROCESSOR_ARCHITECTURE% %USERPROFILE%"`); werr == nil {
			comp, arch, profile, perr := parseWinProbe(wout)
			if perr != nil {
				return fmt.Errorf("target answers cmd but not a usable Windows probe: %v", perr)
			}
			return nodeInstallWindows(ssh, cfg, netName, nc, winOpts{
				name: *name, bind: *bind, hub: *hubAddr, dest: *dest, home: *home,
				bin: *bin, src: *src, port: *port,
				controlMode: controlMode, controlToken: nodeControlToken,
				persist: *persist, noStart: *noStart,
				restart: effectiveRestart, noAnnounce: *noAnnounce,
				computer: comp, goarch: arch, profile: profile,
			})
		}
		return uerr
	}
	f := strings.Fields(out)
	if len(f) < 3 {
		return fmt.Errorf("unexpected uname output %q", out)
	}
	goos, goarch, err := sshx.PlatformOf(f[0], f[2])
	if err != nil {
		return err
	}
	if *name == "" {
		short, _, _ := strings.Cut(f[1], ".")
		*name = config.Sanitize(short)
	}
	if !proto.ValidName(*name) {
		return fmt.Errorf("bad node name %q", *name)
	}
	if *name == cfg.HostName {
		return fmt.Errorf("node would be named %q, same as this host — pass --name", *name)
	}
	fmt.Printf("  %s/%s, node name %q\n", goos, goarch, *name)
	if *dest == "" {
		*dest = "$HOME/.local/bin/hive"
	}
	if *home == "" {
		*home = "$HOME/.hive"
	}
	destPath, err := sshx.RemotePath(*dest)
	if err != nil {
		return err
	}
	homePath, err := sshx.RemotePath(*home)
	if err != nil {
		return err
	}
	if !strings.Contains(destPath, "/") {
		return fmt.Errorf("--dest must be a path, got %q", destPath)
	}

	// 2. Get a binary for that platform.
	binPath := *bin
	if binPath == "" {
		if goos == runtime.GOOS && goarch == runtime.GOARCH {
			if binPath, err = os.Executable(); err != nil {
				return err
			}
		} else if binPath, err = crossCompile(goos, goarch, *src); err != nil {
			return err
		}
	}

	// 3. Ship it.
	fmt.Printf("installing %s -> %s:%s ...\n", binPath, target, destPath)
	bf, err := os.Open(binPath)
	if err != nil {
		return err
	}
	defer bf.Close()
	destDir := destPath[:strings.LastIndex(destPath, "/")]
	script := fmt.Sprintf(
		`sh -c 'set -e; mkdir -p "%s"; cat > "%s.tmp"; chmod 755 "%s.tmp"; mv "%s.tmp" "%s"'`,
		destDir, destPath, destPath, destPath, destPath)
	if _, err := ssh.Run(bf, script); err != nil {
		return err
	}

	// 4. Where does the node's daemon listen, and where do we?
	if *bind == "" {
		out, _ := ssh.Run(nil, `sh -c 'tailscale ip -4 2>/dev/null | head -n1 || true'`)
		*bind = strings.TrimSpace(out)
		if *bind == "" {
			return fmt.Errorf("cannot detect the node's tailscale IPv4 — pass --bind ADDR")
		}
	}
	if net.ParseIP(*bind) == nil {
		return fmt.Errorf("bad --bind %q (want an IP address)", *bind)
	}
	hub, err := resolveHubAddr(cfg, *hubAddr)
	if err != nil {
		return err
	}
	warnLoopbackHub(cfg, hub)

	// 5. Write the node's config and network state over the ssh channel.
	fmt.Printf("configuring: host_name=%s bind=%s port=%d net=%s control=%s\n",
		*name, *bind, *port, netName, controlMode)
	nodeCfg, _ := json.MarshalIndent(config.Config{HostName: *name, Bind: *bind, Port: *port}, "", "  ")
	if err := ssh.WriteRemote(homePath, homePath+"/config.json", nodeCfg); err != nil {
		return err
	}
	nodeNet := config.NetConfig{
		Name:     netName,
		MsgToken: nc.MsgToken,
		Hosts:    seedHosts(nc.Hosts, cfg.HostName, hub, *name, *port),
	}
	nodeNet.ControlToken = nodeControlToken
	if controlMode == nodeControlLocal {
		nodeNet.ControlHost = *name
	}
	nodeNetJSON, _ := json.MarshalIndent(nodeNet, "", "  ")
	if err := ssh.WriteRemote(homePath+"/nets/"+netName, homePath+"/nets/"+netName+"/net.json", nodeNetJSON); err != nil {
		return err
	}

	// 6. Start (or restart) the daemon and verify it from here.
	nodeURL := "http://" + net.JoinHostPort(*bind, strconv.Itoa(*port))
	hint := fmt.Sprintf("ssh %s tail %s/daemon.log", target, homePath)
	if *persist {
		desc, err := persistNode(ssh, goos, destPath, homePath, !*noStart)
		if err != nil {
			return err
		}
		fmt.Println("persistence: " + desc)
		if !*noStart {
			if err := waitHealthy(nodeURL, hint); err != nil {
				return err
			}
		}
	} else if *noStart && effectiveRestart && healthOK(nodeURL) {
		// A local-control reinstall rotates the accepted token. Leaving an
		// existing daemon alive would leave the old capability active and the
		// new on-disk token unusable until a later restart.
		if _, err := ssh.Run(nil, `sh -c 'pkill -f "[h]ive daemon" || true; sleep 0.3'`); err != nil {
			return err
		}
		fmt.Println("daemon stopped so the rotated control token takes effect on next start")
	} else if !*noStart {
		running := healthOK(nodeURL)
		if running && !effectiveRestart {
			fmt.Println("daemon already running — the old binary stays in memory (pass --restart to upgrade)")
		} else {
			if running {
				ssh.Run(nil, `sh -c 'pkill -f "[h]ive daemon" || true; sleep 0.3'`)
			}
			startCmd := fmt.Sprintf(
				`sh -c 'HIVE_HOME="%s" nohup "%s" daemon >> "%s/daemon.log" 2>&1 < /dev/null &'`,
				homePath, destPath, homePath)
			if _, err := ssh.Run(nil, startCmd); err != nil {
				return err
			}
			if err := waitHealthy(nodeURL, hint); err != nil {
				return err
			}
		}
	}

	// 7. Tell every hub we know (including our own) about the new host.
	nodeAddr := net.JoinHostPort(*bind, strconv.Itoa(*port))
	if err := announceAll(cfg, netName, nc, *name, nodeAddr, *noAnnounce); err != nil {
		return err
	}

	fmt.Printf("\nnode %q is in the mesh:\n", *name)
	fmt.Printf("  hive agents                        # should reach @%s\n", *name)
	switch controlMode {
	case nodeControlShared:
		fmt.Printf("  hive spawn --host %s w1 -- CMD...\n", *name)
	case nodeControlLocal:
		fmt.Printf("  on %s: hive spawn --grant-control w1 -- CMD...\n", *name)
		fmt.Printf("note: %s has host-local control; the original network control token cannot control it remotely\n", *name)
	}
	if *noStart {
		fmt.Printf("  start it: ssh %s '%s daemon'   (HIVE_HOME=%s if non-default)\n", target, destPath, homePath)
		if controlMode == nodeControlLocal {
			fmt.Printf("note: restart any existing daemon before using the new host-local control token\n")
		}
	}
	if !*persist {
		fmt.Printf("note: the daemon is not persisted across reboots (rerun with --persist)\n")
	}
	return nil
}

const systemdSystemUnit = `[Unit]
Description=hive agent mesh daemon
After=network-online.target
Wants=network-online.target

[Service]
User=%s
Environment=HIVE_HOME=%s
ExecStart=%s daemon
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
`

const systemdUserUnit = `[Unit]
Description=hive agent mesh daemon
After=network-online.target

[Service]
Environment=HIVE_HOME=%s
ExecStart=%s daemon
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`

// launchdPlist args (in order): binary path, HIVE_HOME, PATH, log dir, log
// dir. A GUI LaunchAgent otherwise inherits only /usr/bin:/bin:/usr/sbin:
// /sbin, which excludes Homebrew (where tmux lives) and ~/.local/bin (where
// the claude CLI lives), so the daemon's spawn path can't find either until
// the plist is hand-edited. LC_ALL=C pins the ps-lstart format the liveness
// check compares (see internal/tmux ProcStartEpoch).
const launchdPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.hive.daemon</string>
  <key>ProgramArguments</key><array><string>%s</string><string>daemon</string></array>
  <key>EnvironmentVariables</key><dict><key>HIVE_HOME</key><string>%s</string><key>PATH</key><string>%s</string><key>LC_ALL</key><string>C</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s/daemon.log</string>
  <key>StandardErrorPath</key><string>%s/daemon.log</string>
</dict></plist>
`

// persistNode installs a supervisor for the node's daemon and (when
// startNow) hands the running daemon over to it. Unit files need
// absolute paths, so the node's $HOME is resolved first.
func persistNode(ssh sshx.Runner, goos, dest, home string, startNow bool) (string, error) {
	out, err := ssh.Run(nil, `sh -c 'echo "$HOME"'`)
	if err != nil {
		return "", err
	}
	absHome := strings.TrimSpace(out)
	if absHome == "" || !strings.HasPrefix(absHome, "/") {
		return "", fmt.Errorf("cannot resolve the node's $HOME (got %q)", absHome)
	}
	abs := func(p string) string { return strings.ReplaceAll(p, "$HOME", absHome) }
	if goos == "darwin" {
		return persistLaunchd(ssh, abs(dest), abs(home), absHome)
	}
	return persistSystemd(ssh, abs(dest), abs(home), startNow)
}

// launchdPATH is the PATH a persisted macOS daemon needs to find tmux
// (Homebrew) and claude (~/.local/bin). It seeds the standard locations
// from the node's home, then unions in the node's login-shell PATH (which
// sources ~/.zprofile, so a nonstandard Homebrew/claude prefix is picked
// up too) when the probe yields one.
func launchdPATH(ssh sshx.Runner, home string) string {
	base := "/opt/homebrew/bin:/opt/homebrew/sbin:" + home + "/.local/bin:" + home + "/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	// Login-shell PATH probe; \$PATH is escaped so the outer sh -c leaves it
	// for the login shell to expand. Best-effort — base alone already works.
	out, err := ssh.Run(nil, `sh -c 'S=${SHELL:-/bin/sh}; "$S" -lc "printf %s \"\$PATH\"" 2>/dev/null'`)
	login := strings.TrimSpace(out)
	path := base
	if err == nil && strings.Contains(login, "/") {
		path = login + ":" + base
	}
	// The value lands in a plist <string>; escape XML metacharacters so a
	// dir name containing & < > can't corrupt the file.
	return xmlEscape(path)
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func persistSystemd(ssh sshx.Runner, dest, home string, startNow bool) (string, error) {
	if out, _ := ssh.Run(nil, `sh -c '[ -d /run/systemd/system ] && command -v systemctl >/dev/null && echo yes || true'`); !strings.Contains(out, "yes") {
		return "", fmt.Errorf("--persist needs systemd on the node (not running there); start the daemon your own way or drop --persist")
	}
	out, err := ssh.Run(nil, `sh -c 'if [ "$(id -u)" = 0 ]; then echo root; elif sudo -n true 2>/dev/null; then echo sudo; else echo user; fi; id -un'`)
	if err != nil {
		return "", err
	}
	f := strings.Fields(out)
	if len(f) < 2 {
		return "", fmt.Errorf("privilege probe returned %q", out)
	}
	mode, user := f[0], f[1]

	now := ""
	if startNow {
		now = " --now"
	}
	if mode == "user" {
		unit := fmt.Sprintf(systemdUserUnit, home, dest)
		if err := ssh.WriteRemote("$HOME/.config/systemd/user", "$HOME/.config/systemd/user/hive.service", []byte(unit)); err != nil {
			return "", err
		}
		script := fmt.Sprintf(
			`sh -c 'set -e; export XDG_RUNTIME_DIR="/run/user/$(id -u)"; systemctl --user daemon-reload; pkill -f "[h]ive daemon" 2>/dev/null || true; systemctl --user enable%s hive; loginctl enable-linger "$(id -un)" 2>/dev/null || true'`, now)
		if _, err := ssh.Run(nil, script); err != nil {
			return "", err
		}
		desc := "systemd user unit (~/.config/systemd/user/hive.service)"
		if lo, _ := ssh.Run(nil, `sh -c 'loginctl show-user "$(id -un)" -p Linger 2>/dev/null || true'`); !strings.Contains(lo, "Linger=yes") {
			desc += " — WARNING: lingering is off, so it only runs while you're logged in (a root shell can fix it: loginctl enable-linger " + user + ")"
		}
		return desc, nil
	}

	pre := ""
	if mode == "sudo" {
		pre = "sudo "
	}
	unit := fmt.Sprintf(systemdSystemUnit, user, home, dest)
	if err := ssh.WriteRemote("/tmp", "/tmp/hive.service.tmp", []byte(unit)); err != nil {
		return "", err
	}
	script := fmt.Sprintf(
		`sh -c 'set -e; %smv /tmp/hive.service.tmp /etc/systemd/system/hive.service; %schmod 644 /etc/systemd/system/hive.service; %ssystemctl daemon-reload; pkill -f "[h]ive daemon" 2>/dev/null || true; %ssystemctl enable%s hive'`,
		pre, pre, pre, pre, now)
	if _, err := ssh.Run(nil, script); err != nil {
		return "", err
	}
	return fmt.Sprintf("systemd system unit (/etc/systemd/system/hive.service, User=%s, via %s)", user, mode), nil
}

func persistLaunchd(ssh sshx.Runner, dest, home, userHome string) (string, error) {
	plist := fmt.Sprintf(launchdPlist, dest, home, launchdPATH(ssh, userHome), home, home)
	if err := ssh.WriteRemote("$HOME/Library/LaunchAgents", "$HOME/Library/LaunchAgents/com.hive.daemon.plist", []byte(plist)); err != nil {
		return "", err
	}
	script := `sh -c 'set -e; pkill -f "[h]ive daemon" 2>/dev/null || true; launchctl bootout "gui/$(id -u)/com.hive.daemon" 2>/dev/null || true; launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.hive.daemon.plist" 2>/dev/null || launchctl load -w "$HOME/Library/LaunchAgents/com.hive.daemon.plist"'`
	if _, err := ssh.Run(nil, script); err != nil {
		return "", fmt.Errorf("%v — a LaunchAgent needs a logged-in user session; it will activate at next login", err)
	}
	return "launchd agent (~/Library/LaunchAgents/com.hive.daemon.plist)", nil
}

// resolveNetConfig picks the network like client.Resolve does (flag,
// $HIVE_NET, else the sole local net) but returns the raw tokens.
func resolveNetConfig(netFlag string) (string, config.NetConfig, error) {
	name := netFlag
	if name == "" {
		name = os.Getenv("HIVE_NET")
	}
	if name == "" {
		nets, _ := config.ListNets()
		switch len(nets) {
		case 1:
			name = nets[0]
		case 0:
			return "", config.NetConfig{}, fmt.Errorf("no networks configured — run: hive net create <name>")
		default:
			return "", config.NetConfig{}, fmt.Errorf("multiple networks (%s) — pass --net", strings.Join(nets, ", "))
		}
	}
	nc, err := config.LoadNet(name)
	if err != nil {
		return "", config.NetConfig{}, fmt.Errorf("network %q not found here", name)
	}
	return name, nc, nil
}

// sshx.Runner shells out to ssh with batch-mode settings; the command is
// one string parsed by the remote shell (we only ever wrap POSIX
// sh -c '...' scripts that contain no single quotes). When ctlPath is set,
// every run/scp shares one multiplexed connection (ControlMaster), so an
// install's 6-11 steps pay a single handshake instead of one each.
// resolveHubAddr is this hub's address as reachable from the node:
// the flag if given, else the local tailscale IPv4 + configured port.
func resolveHubAddr(cfg config.Config, flag string) (string, error) {
	if flag != "" {
		if _, _, err := net.SplitHostPort(flag); err != nil {
			return "", fmt.Errorf("bad --hub %q: %v", flag, err)
		}
		return flag, nil
	}
	ip := tailnetIPLocal()
	if ip == "" {
		return "", fmt.Errorf("cannot detect this host's tailscale IPv4 — pass --hub ADDR:PORT")
	}
	return net.JoinHostPort(ip, strconv.Itoa(cfg.Port)), nil
}

func warnLoopbackHub(cfg config.Config, hub string) {
	if cfg.Bind == "127.0.0.1" || cfg.Bind == "::1" {
		host, _, _ := net.SplitHostPort(hub)
		fmt.Printf("WARNING: this host's daemon binds %s — the node cannot reach it.\n", cfg.Bind)
		fmt.Printf("         restart it with: hive daemon --bind %s\n", host)
	}
}

// waitHealthy polls the node's health endpoint from the operator side.
func waitHealthy(nodeURL, hint string) error {
	deadline := time.Now().Add(8 * time.Second)
	for !healthOK(nodeURL) {
		if time.Now().After(deadline) {
			return fmt.Errorf("node daemon never became healthy at %s — check: %s", nodeURL, hint)
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("daemon healthy at %s\n", nodeURL)
	return nil
}

// announceAll adds the new host to every hub in the local hosts list
// (including our own; if our daemon is down the on-disk list is fixed
// directly). Failures print the manual command, never abort.
func announceAll(cfg config.Config, netName string, nc config.NetConfig, nodeName, nodeAddr string, skip bool) error {
	if skip {
		return nil
	}
	if nc.ControlToken == "" {
		fmt.Printf("no control token here — on each host run: hive hosts add %s %s\n", nodeName, nodeAddr)
		return nil
	}
	for host, addr := range nc.Hosts {
		if host == nodeName {
			continue
		}
		controlToken := nc.ControlFor(host)
		if controlToken == "" {
			fmt.Printf("announce to %-16s skipped (no capability for that host) — run there: hive hosts add %s %s\n",
				host, nodeName, nodeAddr)
			continue
		}
		err := postHostsAdd(addr, netName, controlToken, nodeName, nodeAddr)
		switch {
		case err == nil:
			fmt.Printf("announced to %-16s %s\n", host, addr)
		case host == cfg.HostName:
			nc.Hosts[nodeName] = nodeAddr
			if err := config.SaveNet(nc); err != nil {
				return err
			}
			fmt.Printf("announced to %-16s (daemon down — updated net.json directly)\n", host)
		default:
			fmt.Printf("announce to %-16s FAILED (%v) — run there: hive hosts add %s %s\n",
				host, err, nodeName, nodeAddr)
		}
	}
	return nil
}

// crossCompile builds cmd/hive for the target platform from src, which
// must be this module's source tree.
func crossCompile(goos, goarch, src string) (string, error) {
	mod, err := os.ReadFile(src + "/go.mod")
	if err != nil || !strings.Contains(string(mod), "module github.com/benhynes/hive") {
		return "", fmt.Errorf("node is %s/%s but %q is not the hive source tree — pass --src or --bin", goos, goarch, src)
	}
	out := fmt.Sprintf("%s/hive-%s-%s", os.TempDir(), goos, goarch)
	if goos == "windows" {
		out += ".exe"
	}
	c := exec.Command("go", "build", "-o", out, "./cmd/hive")
	c.Dir = src
	c.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	if b, err := c.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cross-compile %s/%s: %v: %s", goos, goarch, err, strings.TrimSpace(string(b)))
	}
	return out, nil
}

// seedHosts is the new node's initial hosts map: everything this host
// knows, our own loopback entry replaced by the address the node can
// reach us at, plus the node's own loopback self-entry.
func seedHosts(local map[string]string, selfName, hubAddr, nodeName string, nodePort int) map[string]string {
	out := make(map[string]string, len(local)+2)
	for k, v := range local {
		out[k] = v
	}
	out[selfName] = hubAddr
	out[nodeName] = fmt.Sprintf("127.0.0.1:%d", nodePort)
	return out
}

// tailnetIPLocal finds this machine's tailscale IPv4: the CLI if on
// PATH, else the first interface address in CGNAT 100.64.0.0/10.
func tailnetIPLocal() string {
	if out, err := exec.Command("tailscale", "ip", "-4").Output(); err == nil {
		if ip := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]); ip != "" {
			return ip
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if isCGNAT(ipn.IP) {
			return ipn.IP.To4().String()
		}
	}
	return ""
}

// isCGNAT reports whether ip is in 100.64.0.0/10 (tailscale's range).
func isCGNAT(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127
}

func healthOK(base string) bool {
	hc := &http.Client{Timeout: 2 * time.Second}
	resp, err := hc.Get(base + "/v1/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// postHostsAdd registers name->addr in one hub's local hosts list.
func postHostsAdd(hubAddr, netName, controlToken, name, addr string) error {
	body, _ := json.Marshal(map[string]string{"op": "add", "name": name, "addr": addr})
	req, err := http.NewRequest("POST", "http://"+hubAddr+"/v1/nets/"+netName+"/hosts", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+controlToken)
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 4 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s", e.Error)
	}
	return nil
}
