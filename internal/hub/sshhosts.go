package hub

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/proto"
	"github.com/benhynes/hive/internal/sshx"
)

// selfBinary is the absolute path of the running hive binary, for shipping to
// a platform-matching SSH host.
func selfBinary() (string, error) { return os.Executable() }

// healthOKURL reports whether a hub answers /v1/health at base.
func healthOKURL(base string) bool {
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(base + "/v1/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func requireHealthFeatureURL(base, feature string) error {
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(base + "/v1/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: %s", resp.Status)
	}
	var health struct {
		Features []string `json:"features"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return err
	}
	for _, advertised := range health.Features {
		if advertised == feature {
			return nil
		}
	}
	return fmt.Errorf("daemon does not advertise %q", feature)
}

// sshManager owns the lifecycle of registered SSH hosts for one hub: bringing
// up a transient loopback daemon on the remote over an SSH ControlMaster,
// wiring the loopback port-forwards that carry hub↔hub traffic, and tearing it
// all down. It is the origin-side counterpart to a tailnet peer — once up, an
// SSH host is reached through a normal hosts entry (a loopback forward), so the
// existing spawn/control/delivery paths work unchanged.
type sshManager struct {
	h  *Hub
	mu sync.Mutex
	up map[string]*sshConn // key: net "\x00" hostname
}

// sshConn is one live SSH host: the master connection, the remote daemon it
// brought up, and the loopback forwards that reach it.
type sshConn struct {
	runner    sshx.Runner
	cleanup   func()
	localPort int    // origin-side loopback port -> remote daemon (via -L)
	remoteURL string // "127.0.0.1:<localPort>": how this hub reaches the remote hub
	ctlToken  string // host-local control token the remote accepts (for forwarded control ops)
	agents    int    // live agent count; teardown when it hits 0 (ephemeral)
}

func newSSHManager(h *Hub) *sshManager {
	return &sshManager{h: h, up: map[string]*sshConn{}}
}

func connKey(netName, host string) string { return netName + "\x00" + host }

// ensureUp brings the SSH host up if it isn't already and returns its
// origin-reachable hub address and the control token the remote accepts.
// Idempotent: a warm host returns immediately. A per-host lock serializes
// concurrent first-spawns so only one bring-up happens.
func (m *sshManager) ensureUp(n *network, host string, sh config.SSHHost) (url, ctlToken string, err error) {
	key := connKey(n.name, host)
	m.mu.Lock()
	if c, ok := m.up[key]; ok {
		m.mu.Unlock()
		return c.remoteURL, c.ctlToken, nil
	}
	m.mu.Unlock()

	c, err := m.bringUp(n, host, sh)
	if err != nil {
		return "", "", err
	}
	m.mu.Lock()
	// Lost a race? keep the winner, tear down ours.
	if existing, ok := m.up[key]; ok {
		m.mu.Unlock()
		c.cleanup()
		return existing.remoteURL, existing.ctlToken, nil
	}
	m.up[key] = c
	m.mu.Unlock()
	return c.remoteURL, c.ctlToken, nil
}

// bringUp performs the full first-spawn sequence: connect, preflight, ship the
// binary, start the transient daemon, join it to the net, and open the
// loopback forwards. See docs/ssh-hosts-design.md §3.
func (m *sshManager) bringUp(n *network, host string, sh config.SSHHost) (*sshConn, error) {
	runner, cleanup := sshx.NewRunner(sh.Target)
	runner.Identity = sh.Identity
	ok := false
	defer func() {
		if !ok {
			cleanup()
		}
	}()

	// 1-2. Preflight: reach the host and learn its platform.
	uname, err := runner.Run(nil, "uname -s -m")
	if err != nil {
		return nil, fmt.Errorf("ssh preflight: %w", err)
	}
	parts := strings.Fields(uname)
	if len(parts) < 2 {
		return nil, fmt.Errorf("unexpected uname output %q", strings.TrimSpace(uname))
	}
	if _, _, err := sshx.PlatformOf(parts[0], parts[1]); err != nil {
		return nil, err
	}

	home := sh.Home
	if home == "" {
		home = "~/.hive"
	}
	homeRP, err := sshx.RemotePath(home)
	if err != nil {
		return nil, err
	}
	remotePort := sh.Port
	if remotePort == 0 {
		remotePort = 7777
	}
	binPath := homeRP + "/bin/hive" // version-pinned dest under the remote HIVE_HOME

	// 3. Ship the binary to binPath if absent.
	if err := m.ensureBinary(runner, sh, binPath); err != nil {
		return nil, err
	}

	// 4. Write the remote daemon's config (its host_name = this SSH host's
	//    name, so agents are stamped name@<host> and routing matches), then
	//    start it detached on the remote loopback. Idempotent: health-check
	//    first, start only if down. nohup + </dev/null lets it outlive the ssh
	//    command; no supervisor (transient).
	cfgJSON := fmt.Sprintf(`{"host_name":%q,"bind":"127.0.0.1","port":%d}`, host, remotePort)
	if err := runner.WriteRemote(homeRP, homeRP+"/config.json", []byte(cfgJSON)); err != nil {
		return nil, fmt.Errorf("write remote config: %w", err)
	}
	// ( ... & ) double-forks: the subshell starts the daemon detached and exits
	// immediately, so no grandchild inherits — and thus holds open — the ssh
	// command's stdout pipe (which would hang runner.Run forever).
	start := fmt.Sprintf(
		`sh -c 'curl -sf http://127.0.0.1:%d/v1/health >/dev/null 2>&1 || ( HIVE_HOME=%s nohup %s daemon >%s/daemon.log 2>&1 </dev/null & )'`,
		remotePort, homeRP, binPath, homeRP)
	if _, err := runner.Run(nil, start); err != nil {
		return nil, fmt.Errorf("start remote daemon: %w", err)
	}

	// 5a. -L forward: an origin loopback port -> the remote daemon.
	localPort, err := freeLoopbackPort()
	if err != nil {
		return nil, err
	}
	if err := runner.ForwardL(localPort, remotePort); err != nil {
		return nil, fmt.Errorf("forward -L: %w", err)
	}
	remoteURL := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))

	// Wait for the daemon to answer through the forward.
	if err := waitHealthyURL("http://"+remoteURL, 8*time.Second); err != nil {
		return nil, err
	}
	if err := requireHealthFeatureURL("http://"+remoteURL, "explicit_nudge"); err != nil {
		return nil, fmt.Errorf("remote daemon compatibility: %w (upgrade/restart hive)", err)
	}

	// 5b. -R reverse forward: let the remote hub reach THIS hub's loopback, and
	//     join the remote to the net so hub↔hub delivery works both ways.
	fmt.Fprintln(os.Stderr, "BRINGUP: healthy, joinRemote (-R)")
	ctlToken, err := m.joinRemote(runner, n, host, homeRP, remotePort, remoteURL)
	if err != nil {
		return nil, err
	}

	ok = true
	return &sshConn{runner: runner, cleanup: cleanup, localPort: localPort, remoteURL: remoteURL, ctlToken: ctlToken}, nil
}

// ensureBinary ships the hive binary to the remote if it's absent. For a
// platform matching the origin it self-copies the running binary; otherwise it
// requires a prebuilt binary via SSHHost.Bin (cross-compile-on-demand is a
// P2.1 add — the CLI's node install already has that logic to promote later).
func (m *sshManager) ensureBinary(runner sshx.Runner, sh config.SSHHost, binPath string) error {
	// Already there and runnable?
	if _, err := runner.Run(nil, fmt.Sprintf("sh -c '%s --help >/dev/null 2>&1 && echo ok'", binPath)); err == nil {
		return nil
	}
	local := sh.Bin
	if local == "" {
		self, err := selfBinary()
		if err != nil {
			return fmt.Errorf("no --bin for SSH host and cannot self-copy: %w", err)
		}
		local = self
	}
	dir := binPath[:strings.LastIndex(binPath, "/")]
	if _, err := runner.Run(nil, fmt.Sprintf(`sh -c 'mkdir -p %s'`, dir)); err != nil {
		return err
	}
	if err := runner.SCP(local, binPath+".tmp"); err != nil {
		return err
	}
	_, err := runner.Run(nil, fmt.Sprintf(`sh -c 'chmod +x %s.tmp && mv %s.tmp %s'`, binPath, binPath, binPath))
	return err
}

// joinRemote writes the net's tokens into the remote daemon's config over the
// reverse forward and registers reciprocal hosts entries, so the two hubs can
// deliver to each other. The remote joins pointing at this hub via the -R
// forward's remote-side loopback port.
func (m *sshManager) joinRemote(runner sshx.Runner, n *network, host, homeRP string, remotePort int, remoteURL string) (string, error) {
	n.mu.Lock()
	nc := n.cfg
	n.mu.Unlock()

	// -R: a remote loopback port reaches this hub's real port. Pick a specific
	// free port on the remote first — OpenSSH's `-O forward` (mux) can't do
	// dynamic (port 0) allocation, so we can't ask sshd to choose.
	rp, err := freeRemotePort(runner)
	if err != nil {
		return "", err
	}
	if _, err := runner.ForwardR(rp, m.h.Cfg.Port); err != nil {
		return "", fmt.Errorf("forward -R: %w", err)
	}

	// Mint a fresh control token scoped to the remote host, so the origin's
	// network-wide control token is never copied onto the box (design §6). The
	// origin keeps it in memory to authenticate forwarded control ops.
	ctlToken := proto.NewToken()

	// The remote's net.json: shared msg token, a host-local control token,
	// hosts pointing back at us over -R, and its own loopback self-entry.
	remoteNet := config.NetConfig{
		Name:         n.name,
		MsgToken:     nc.MsgToken,
		ControlToken: ctlToken,
		ControlHost:  host,
		Hosts: map[string]string{
			m.h.Cfg.HostName: net.JoinHostPort("127.0.0.1", strconv.Itoa(rp)),
			host:             net.JoinHostPort("127.0.0.1", strconv.Itoa(remotePort)),
		},
	}
	b, _ := json.Marshal(remoteNet)
	netDir := homeRP + "/nets/" + n.name
	if err := runner.WriteRemote(netDir, netDir+"/net.json", b); err != nil {
		return "", fmt.Errorf("write remote net.json: %w", err)
	}
	// The remote daemon loads net.json lazily per request, so no restart needed.

	// This hub learns the remote as a peer at its -L loopback address.
	n.mu.Lock()
	if n.cfg.Hosts == nil {
		n.cfg.Hosts = map[string]string{}
	}
	n.cfg.Hosts[host] = remoteURL
	err = config.SaveNet(n.cfg)
	n.mu.Unlock()
	return ctlToken, err
}

// release decrements the live-agent count for a host and, at zero, tears it
// down (ephemeral policy). Warm/idle-timeout is a P3 refinement.
func (m *sshManager) release(netName, host string) {
	key := connKey(netName, host)
	m.mu.Lock()
	c, ok := m.up[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	c.agents--
	if c.agents > 0 {
		m.mu.Unlock()
		return
	}
	delete(m.up, key)
	m.mu.Unlock()
	c.cleanup()
}

// acquire records a new live agent on a host.
func (m *sshManager) acquire(netName, host string) {
	m.mu.Lock()
	if c, ok := m.up[connKey(netName, host)]; ok {
		c.agents++
	}
	m.mu.Unlock()
}

// teardown closes one live SSH host's tunnel + master connection regardless of
// agent count (used by `hosts rm-ssh`). A no-op if it isn't up.
func (m *sshManager) teardown(netName, host string) {
	key := connKey(netName, host)
	m.mu.Lock()
	c, ok := m.up[key]
	delete(m.up, key)
	m.mu.Unlock()
	if ok {
		c.cleanup()
	}
}

// shutdown tears down every live SSH host (origin daemon stopping).
func (m *sshManager) shutdown() {
	m.mu.Lock()
	conns := make([]*sshConn, 0, len(m.up))
	for k, c := range m.up {
		conns = append(conns, c)
		delete(m.up, k)
	}
	m.mu.Unlock()
	for _, c := range conns {
		c.cleanup()
	}
}

// spawnOntoSSH resolves the profile on the origin (where its files live) into
// a self-contained provisioning spec, brings the SSH host up, and forwards a
// normal spawn to its hub over the -L tunnel. The remote daemon mints the
// agent token and applies the spec; the returned agent id is name@<remote>.
func (h *Hub) spawnOntoSSH(w http.ResponseWriter, r *http.Request, n *network, id ident, req spawnReq) {
	n.mu.Lock()
	sh, ok := n.cfg.SSHHosts[req.SSHHost]
	n.mu.Unlock()
	if !ok {
		httpErr(w, 400, "unknown SSH host %q", req.SSHHost)
		return
	}

	profName := req.Profile
	if profName == "" {
		profName = sh.Profile
	}
	var prof config.SpawnProfile
	if profName != "" {
		var err error
		if prof, err = config.LoadProfile(profName); err != nil {
			httpErr(w, 400, "profile %q: %v", profName, err)
			return
		}
	}
	cmd := req.Cmd
	if len(cmd) == 0 {
		cmd = prof.Runtime
	}
	if len(cmd) == 0 {
		httpErr(w, 400, "empty command (give a `-- CMD` or a profile with a runtime)")
		return
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = prof.Cwd
	}
	spec, err := buildProvision(prof)
	if err != nil {
		httpErr(w, 500, "provision: %v", err)
		return
	}

	url, ctlToken, err := h.ssh.ensureUp(n, req.SSHHost, sh)
	if err != nil {
		httpErr(w, 502, "ssh host %q: %v", req.SSHHost, err)
		return
	}
	// This must precede the mutating spawn. An older remote daemon implicitly
	// nudged every pane, and discovering that from the spawn response is too
	// late (and cannot be safely cleaned up for idempotent persistent spawns).
	if err := requireHealthFeatureURL("http://"+url, "explicit_nudge"); err != nil {
		httpErr(w, 502, "remote daemon compatibility: %v; upgrade/restart hive", err)
		return
	}

	fwd := spawnReq{
		Name: req.Name, Cmd: cmd, Cwd: cwd, Provision: &spec,
		GrantControl: req.GrantControl, WaitReady: req.WaitReady,
		Nudge: req.Nudge, Headed: req.Headed, Persist: req.Persist,
	}
	var out spawnResp
	if err := postJSON(url, "/v1/nets/"+n.name+"/spawn/v2", ctlToken, fwd, &out, 30*time.Second); err != nil {
		httpErr(w, 502, "forward spawn to %q: %v", req.SSHHost, err)
		return
	}
	if out.NudgePolicy != "explicit" || out.Nudge != req.Nudge {
		httpErr(w, 502, "remote daemon advertised explicit terminal nudging but did not honor the requested policy")
		return
	}
	h.ssh.acquire(n.name, req.SSHHost)
	n.auditLine(h.actor(r, id), "spawn-ssh", out.Agent, "host="+req.SSHHost)
	writeJSON(w, 200, out)
}

// postJSON is an authenticated hub→hub POST with a caller-set timeout (longer
// than h.rpc's, since a forwarded wait-ready spawn can take ~15s). base is
// host:port; the scheme is http.
func postJSON(base, path, token string, in, out any, timeout time.Duration) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", "http://"+base+path, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
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
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// freeRemotePort probes an unused 127.0.0.1 port on the remote (for the -R
// forward, whose listen port lives there). Uses python3, which every supported
// SSH host has; a tiny TOCTOU window is acceptable (bring-up would just error
// and retry). Works for the loopback shim too (runs locally there).
func freeRemotePort(runner sshx.Runner) (int, error) {
	out, err := runner.Run(nil,
		`python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'`)
	if err != nil {
		return 0, fmt.Errorf("probe remote free port (needs python3 on the host): %w", err)
	}
	p, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || p == 0 {
		return 0, fmt.Errorf("bad remote free port %q", strings.TrimSpace(out))
	}
	return p, nil
}

// freeLoopbackPort asks the OS for an unused 127.0.0.1 port, avoiding a fixed
// range so multiple origin hubs on one machine don't collide.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitHealthyURL polls a hub /v1/health until it answers or the deadline.
func waitHealthyURL(base string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		if healthOKURL(base) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("remote hub never became healthy at %s", base)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
