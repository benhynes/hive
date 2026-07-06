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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/proto"
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

func nodeInstall(args []string) error {
	fs := flags("node install", args)
	name := fs.String("name", "", "node's hive host name (default: its hostname)")
	bind := fs.String("bind", "", "address the node's daemon binds (default: its tailscale IPv4)")
	port := fs.Int("port", 7777, "node's daemon port")
	hubAddr := fs.String("hub", "", "this hub's addr:port as reachable FROM the node (default: local tailscale IPv4 + local port)")
	dest := fs.String("dest", "$HOME/.local/bin/hive", "remote path for the binary")
	home := fs.String("home", "$HOME/.hive", "remote hive state dir")
	bin := fs.String("bin", "", "prebuilt binary to ship (default: self-copy, or cross-compile from --src)")
	src := fs.String("src", ".", "hive source dir for cross-compiling")
	msgOnly := fs.Bool("msg-only", false, "don't ship the control token (node can never control anyone)")
	noStart := fs.Bool("no-start", false, "install and configure only; don't start the daemon")
	restart := fs.Bool("restart", false, "restart the node's daemon if one is already running (upgrades)")
	noAnnounce := fs.Bool("no-announce", false, "don't add the node to the other hubs' hosts lists")
	fs.Parse2()
	target := fs.pos(0)
	if target == "" {
		return fmt.Errorf("usage: hive node install [flags] <ssh-target>   (flags before the target)")
	}
	destPath, err := remotePath(*dest)
	if err != nil {
		return err
	}
	homePath, err := remotePath(*home)
	if err != nil {
		return err
	}
	if !strings.Contains(destPath, "/") {
		return fmt.Errorf("--dest must be a path, got %q", destPath)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	netName, nc, err := resolveNetConfig(*fs.net)
	if err != nil {
		return err
	}
	if nc.ControlToken == "" && !*msgOnly {
		fmt.Println("note: this host holds no control token — installing the node msg-only")
		*msgOnly = true
	}

	ssh := sshRunner{target: target}

	// 1. What are we installing onto?
	fmt.Printf("probing %s ...\n", target)
	out, err := ssh.run(nil, `uname -s -n -m`)
	if err != nil {
		return err
	}
	f := strings.Fields(out)
	if len(f) < 3 {
		return fmt.Errorf("unexpected uname output %q", out)
	}
	goos, goarch, err := platformOf(f[0], f[2])
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
	if _, err := ssh.run(bf, script); err != nil {
		return err
	}

	// 4. Where does the node's daemon listen, and where do we?
	if *bind == "" {
		out, _ := ssh.run(nil, `sh -c 'tailscale ip -4 2>/dev/null | head -n1 || true'`)
		*bind = strings.TrimSpace(out)
		if *bind == "" {
			return fmt.Errorf("cannot detect the node's tailscale IPv4 — pass --bind ADDR")
		}
	}
	if net.ParseIP(*bind) == nil {
		return fmt.Errorf("bad --bind %q (want an IP address)", *bind)
	}
	if *hubAddr == "" {
		ip := tailnetIPLocal()
		if ip == "" {
			return fmt.Errorf("cannot detect this host's tailscale IPv4 — pass --hub ADDR:PORT")
		}
		*hubAddr = net.JoinHostPort(ip, strconv.Itoa(cfg.Port))
	}
	if _, _, err := net.SplitHostPort(*hubAddr); err != nil {
		return fmt.Errorf("bad --hub %q: %v", *hubAddr, err)
	}
	if cfg.Bind == "127.0.0.1" || cfg.Bind == "::1" {
		fmt.Printf("WARNING: this host's daemon binds %s — the node cannot reach it.\n", cfg.Bind)
		fmt.Printf("         restart it with: hive daemon --bind %s\n", strings.Split(*hubAddr, ":")[0])
	}

	// 5. Write the node's config and network state over the ssh channel.
	fmt.Printf("configuring: host_name=%s bind=%s port=%d net=%s msg_only=%v\n",
		*name, *bind, *port, netName, *msgOnly)
	nodeCfg, _ := json.MarshalIndent(config.Config{HostName: *name, Bind: *bind, Port: *port}, "", "  ")
	if err := writeRemote(ssh, homePath, homePath+"/config.json", nodeCfg); err != nil {
		return err
	}
	nodeNet := config.NetConfig{
		Name:     netName,
		MsgToken: nc.MsgToken,
		Hosts:    seedHosts(nc.Hosts, cfg.HostName, *hubAddr, *name, *port),
	}
	if !*msgOnly {
		nodeNet.ControlToken = nc.ControlToken
	}
	nodeNetJSON, _ := json.MarshalIndent(nodeNet, "", "  ")
	if err := writeRemote(ssh, homePath+"/nets/"+netName, homePath+"/nets/"+netName+"/net.json", nodeNetJSON); err != nil {
		return err
	}

	// 6. Start (or restart) the daemon and verify it from here.
	nodeURL := "http://" + net.JoinHostPort(*bind, strconv.Itoa(*port))
	if !*noStart {
		running := healthOK(nodeURL)
		if running && !*restart {
			fmt.Println("daemon already running — the old binary stays in memory (pass --restart to upgrade)")
		} else {
			if running {
				ssh.run(nil, `sh -c 'pkill -f "[h]ive daemon" || true; sleep 0.3'`)
			}
			startCmd := fmt.Sprintf(
				`sh -c 'HIVE_HOME="%s" nohup "%s" daemon >> "%s/daemon.log" 2>&1 < /dev/null &'`,
				homePath, destPath, homePath)
			if _, err := ssh.run(nil, startCmd); err != nil {
				return err
			}
			deadline := time.Now().Add(5 * time.Second)
			for !healthOK(nodeURL) {
				if time.Now().After(deadline) {
					return fmt.Errorf("node daemon never became healthy at %s — check: ssh %s tail %s/daemon.log",
						nodeURL, target, homePath)
				}
				time.Sleep(100 * time.Millisecond)
			}
			fmt.Printf("daemon healthy at %s\n", nodeURL)
		}
	}

	// 7. Tell every hub we know (including our own) about the new host.
	nodeAddr := net.JoinHostPort(*bind, strconv.Itoa(*port))
	if !*noAnnounce && nc.ControlToken != "" {
		for host, addr := range nc.Hosts {
			if host == *name {
				continue
			}
			err := postHostsAdd(addr, netName, nc.ControlToken, *name, nodeAddr)
			switch {
			case err == nil:
				fmt.Printf("announced to %-16s %s\n", host, addr)
			case host == cfg.HostName:
				// Our own daemon is down: fix the on-disk list directly.
				nc.Hosts[*name] = nodeAddr
				if err := config.SaveNet(nc); err != nil {
					return err
				}
				fmt.Printf("announced to %-16s (daemon down — updated net.json directly)\n", host)
			default:
				fmt.Printf("announce to %-16s FAILED (%v) — run there: hive hosts add %s %s\n",
					host, err, *name, nodeAddr)
			}
		}
	} else if !*noAnnounce {
		fmt.Printf("no control token here — on each host run: hive hosts add %s %s\n", *name, nodeAddr)
	}

	fmt.Printf("\nnode %q is in the mesh:\n", *name)
	fmt.Printf("  hive agents                        # should reach @%s\n", *name)
	fmt.Printf("  hive spawn --host %s w1 -- CMD...\n", *name)
	if *noStart {
		fmt.Printf("  start it: ssh %s '%s daemon'   (HIVE_HOME=%s if non-default)\n", target, destPath, homePath)
	}
	fmt.Printf("note: the daemon is not persisted across reboots (add systemd/launchd yourself if needed)\n")
	return nil
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

// sshRunner shells out to ssh with batch-mode settings; the command is
// one string parsed by the remote shell (we only ever wrap POSIX
// sh -c '...' scripts that contain no single quotes).
type sshRunner struct{ target string }

func (s sshRunner) run(stdin *os.File, cmd string) (string, error) {
	c := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", s.target, cmd)
	if stdin != nil {
		c.Stdin = stdin
	}
	var out, errb strings.Builder
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("ssh %s: %v: %s", s.target, err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// writeRemote streams content into a remote file under dir, 0600, via
// the ssh channel (never argv).
func writeRemote(ssh sshRunner, dir, path string, content []byte) error {
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
	_, err = ssh.run(r, script)
	return err
}

var remotePathRe = regexp.MustCompile(`^[A-Za-z0-9_./$-]+$`)

// remotePath normalizes a --dest/--home value for safe embedding in a
// double-quoted remote sh script. Leading ~/ becomes $HOME/.
func remotePath(p string) (string, error) {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		p = "$HOME/" + rest
	}
	if !remotePathRe.MatchString(p) {
		return "", fmt.Errorf("remote path %q may only contain [A-Za-z0-9_./$-]", p)
	}
	return p, nil
}

// platformOf maps uname output to GOOS/GOARCH.
func platformOf(unameS, unameM string) (string, string, error) {
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

// crossCompile builds cmd/hive for the target platform from src, which
// must be this module's source tree.
func crossCompile(goos, goarch, src string) (string, error) {
	mod, err := os.ReadFile(src + "/go.mod")
	if err != nil || !strings.Contains(string(mod), "module github.com/benhynes/hive") {
		return "", fmt.Errorf("node is %s/%s but %q is not the hive source tree — pass --src or --bin", goos, goarch, src)
	}
	out := fmt.Sprintf("%s/hive-%s-%s", os.TempDir(), goos, goarch)
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
		ip4 := ipn.IP.To4()
		if ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return ip4.String()
		}
	}
	return ""
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