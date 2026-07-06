// End-to-end tests: build the real binary, run two daemons on this
// machine (separate HIVE_HOME, distinct host names, one shared network),
// and drive everything through the CLI. Control ops run against a real
// tmux server on a dedicated socket, like internal/tmux's tests.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const tmuxSocket = "hive-e2e"

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func hiveBin(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hive-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "hive")
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build: %v: %s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// hub is one daemon under test.
type hubProc struct {
	home, host string
	port       int
	msgTok     string
	ctlTok     string
}

func (h *hubProc) addr() string { return fmt.Sprintf("127.0.0.1:%d", h.port) }
func (h *hubProc) url() string  { return "http://" + h.addr() }

// env is the base environment for CLI calls against this hub. extra
// overrides win because later entries take precedence in exec.
func (h *hubProc) env(extra ...string) []string {
	e := append(os.Environ(), "HIVE_HOME="+h.home, "HIVE_TMUX_SOCKET="+tmuxSocket, "TMUX_PANE=")
	return append(e, extra...)
}

// cli runs one hive command, returning stdout. Stderr is attached to the
// error on failure and discarded on success (it carries advisory text).
func cli(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(hiveBin(t), args...)
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("hive %s: %v: %s", strings.Join(args, " "), err, errb.String())
	}
	return out.String(), nil
}

func mustCLI(t *testing.T, env []string, args ...string) string {
	t.Helper()
	out, err := cli(t, env, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func startHub(t *testing.T, host string) *hubProc {
	t.Helper()
	h := &hubProc{home: t.TempDir(), host: host, port: freePort(t)}
	cfg := fmt.Sprintf(`{"host_name":%q,"bind":"127.0.0.1","port":%d}`, host, h.port)
	if err := os.WriteFile(filepath.Join(h.home, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(hiveBin(t), "daemon")
	cmd.Env = h.env()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(h.url() + "/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return h
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("daemon %s never became healthy", host)
	return nil
}

var exportRe = regexp.MustCompile(`(?m)^export (HIVE_[A-Z_]+)=(\S+)$`)

// register registers an agent (no pane — message-only) and returns the
// env for acting as it.
func register(t *testing.T, h *hubProc, name string) []string {
	t.Helper()
	out := mustCLI(t, h.env(), "register", "--name", name)
	exports := map[string]string{}
	for _, m := range exportRe.FindAllStringSubmatch(out, -1) {
		exports[m[1]] = m[2]
	}
	for _, k := range []string{"HIVE_ADDR", "HIVE_NET", "HIVE_AGENT", "HIVE_TOKEN"} {
		if exports[k] == "" {
			t.Fatalf("register %s: missing %s in output:\n%s", name, k, out)
		}
	}
	// A registered agent holds only its own env — no net.json fallback —
	// so point HIVE_HOME at an empty dir, like a genuinely remote agent.
	return append(os.Environ(),
		"HIVE_HOME="+t.TempDir(), "HIVE_TMUX_SOCKET="+tmuxSocket, "TMUX_PANE=",
		"HIVE_ADDR="+exports["HIVE_ADDR"], "HIVE_NET="+exports["HIVE_NET"],
		"HIVE_AGENT="+exports["HIVE_AGENT"], "HIVE_TOKEN="+exports["HIVE_TOKEN"])
}

// httpJSON performs a raw API call, returning status code and body.
func httpJSON(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: two daemons + tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	t.Cleanup(func() { exec.Command("tmux", "-L", tmuxSocket, "kill-server").Run() })

	// ---- hub A: create the network ----
	a := startHub(t, "hosta")
	out := mustCLI(t, a.env(), "net", "create", "dev")
	tokRe := regexp.MustCompile(`(?m)^  (msg|control) token: +([0-9a-f]{64})$`)
	for _, m := range tokRe.FindAllStringSubmatch(out, -1) {
		if m[1] == "msg" {
			a.msgTok = m[2]
		} else {
			a.ctlTok = m[2]
		}
	}
	if a.msgTok == "" || a.ctlTok == "" {
		t.Fatalf("net create output missing tokens:\n%s", out)
	}

	alice := register(t, a, "alice")
	bob := register(t, a, "bob")

	t.Run("send-recv-ack", func(t *testing.T) {
		mustCLI(t, alice, "send", "bob", "hello bob")
		out := mustCLI(t, bob, "recv")
		if !strings.Contains(out, "<alice@hosta>") || !strings.Contains(out, "hello bob") {
			t.Fatalf("recv: %q", out)
		}
		// Acked: a second recv sees nothing new.
		out = mustCLI(t, bob, "recv")
		if strings.Contains(out, "hello bob") {
			t.Fatalf("message not acked, recv again: %q", out)
		}
	})

	t.Run("from-is-stamped-not-spoofable", func(t *testing.T) {
		// Inject a bogus `from` via the raw API using alice's token.
		tok := envVal(t, alice, "HIVE_TOKEN")
		code, body := httpJSON(t, "POST", a.url()+"/v1/nets/dev/send", tok,
			`{"to":"bob@hosta","body":"spoofed","from":"admin@hosta"}`)
		if code != 200 {
			t.Fatalf("send: %d %s", code, body)
		}
		out := mustCLI(t, bob, "recv")
		if !strings.Contains(out, "<alice@hosta>") || strings.Contains(out, "admin@hosta") {
			t.Fatalf("from not stamped from token: %q", out)
		}
	})

	t.Run("layer-enforcement", func(t *testing.T) {
		agentTok := envVal(t, alice, "HIVE_TOKEN")
		// MSG-layer tokens must be rejected on every control endpoint.
		for _, ep := range []struct{ method, path, body string }{
			{"POST", "/v1/nets/dev/spawn", `{"name":"x","cmd":["cat"]}`},
			{"POST", "/v1/nets/dev/keys", `{"agent":"bob","text":"x"}`},
			{"GET", "/v1/nets/dev/read?agent=bob", ""},
			{"POST", "/v1/nets/dev/kill", `{"agent":"bob"}`},
			{"POST", "/v1/nets/dev/hosts", `{"op":"add","name":"evil","addr":"1.2.3.4:1"}`},
		} {
			for tokName, tok := range map[string]string{"agent": agentTok, "net-msg": a.msgTok} {
				code, body := httpJSON(t, ep.method, a.url()+ep.path, tok, ep.body)
				if code != 403 {
					t.Errorf("%s %s with %s token: want 403, got %d %s", ep.method, ep.path, tokName, code, body)
				}
			}
		}
		// Agent tokens must not mint new identities.
		code, body := httpJSON(t, "POST", a.url()+"/v1/nets/dev/register", agentTok, `{"name":"mallory"}`)
		if code != 403 {
			t.Errorf("register with agent token: want 403, got %d %s", code, body)
		}
		// One agent must not read another's inbox.
		code, body = httpJSON(t, "GET", a.url()+"/v1/nets/dev/inbox?agent=bob", agentTok, "")
		if code != 403 {
			t.Errorf("cross-agent inbox read: want 403, got %d %s", code, body)
		}
		// No token at all.
		code, _ = httpJSON(t, "POST", a.url()+"/v1/nets/dev/send", "", `{"to":"bob@hosta","body":"x"}`)
		if code != 401 {
			t.Errorf("no token: want 401, got %d", code)
		}
	})

	t.Run("duplicate-name-rejected", func(t *testing.T) {
		if _, err := cli(t, a.env(), "register", "--name", "alice"); err == nil {
			t.Fatal("second register of a live name should fail")
		}
	})

	t.Run("undeliverable", func(t *testing.T) {
		if _, err := cli(t, alice, "send", "ghost@hosta", "boo"); err == nil {
			t.Fatal("send to unknown agent should fail")
		}
		if _, err := cli(t, alice, "send", "x@nowhere", "boo"); err == nil {
			t.Fatal("send to unknown host should fail")
		}
	})

	t.Run("ask-answer", func(t *testing.T) {
		type askRes struct {
			out string
			err error
		}
		ch := make(chan askRes, 1)
		go func() {
			out, err := cli(t, alice, "ask", "--timeout", "30", "bob", "what is the port?")
			ch <- askRes{out, err}
		}()
		// Bob finds the ask and answers it.
		idRe := regexp.MustCompile(`(?m)^([0-9a-f]{16})  from alice@hosta`)
		var askID string
		deadline := time.Now().Add(10 * time.Second)
		for askID == "" && time.Now().Before(deadline) {
			out := mustCLI(t, bob, "asks")
			if m := idRe.FindStringSubmatch(out); m != nil {
				askID = m[1]
			} else {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if askID == "" {
			t.Fatal("bob never saw the ask")
		}
		mustCLI(t, bob, "answer", askID, "port 7777")
		res := <-ch
		if res.err != nil {
			t.Fatal(res.err)
		}
		if strings.TrimSpace(res.out) != "port 7777" {
			t.Fatalf("ask answer: %q", res.out)
		}
	})

	t.Run("recv-long-poll", func(t *testing.T) {
		cli(t, bob, "recv") // drain: asks/answer peek without acking
		ch := make(chan string, 1)
		go func() {
			out, _ := cli(t, bob, "recv", "--wait", "15")
			ch <- out
		}()
		time.Sleep(300 * time.Millisecond) // let the long-poll arm
		mustCLI(t, alice, "send", "bob", "wake up")
		select {
		case out := <-ch:
			if !strings.Contains(out, "wake up") {
				t.Fatalf("long-poll recv: %q", out)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("long-poll never woke")
		}
	})

	// ---- hub B joins; cross-hub messaging ----
	b := startHub(t, "hostb")
	mustCLI(t, b.env(), "net", "join", "dev",
		"--hub", a.addr(), "--msg-token", a.msgTok, "--control-token", a.ctlTok)
	mustCLI(t, a.env(), "hosts", "add", "hostb", b.addr())

	carol := register(t, b, "carol")

	t.Run("join-learns-peer-name", func(t *testing.T) {
		out := mustCLI(t, b.env(), "hosts", "list")
		if !strings.Contains(out, "hosta") {
			t.Fatalf("join did not learn peer host name:\n%s", out)
		}
	})

	t.Run("cross-hub-send", func(t *testing.T) {
		mustCLI(t, alice, "send", "carol@hostb", "hello across")
		out := mustCLI(t, carol, "recv")
		if !strings.Contains(out, "<alice@hosta>") || !strings.Contains(out, "hello across") {
			t.Fatalf("cross-hub recv: %q", out)
		}
	})

	t.Run("agents-fan-out", func(t *testing.T) {
		out := mustCLI(t, a.env(), "agents")
		for _, want := range []string{"alice@hosta", "bob@hosta", "carol@hostb"} {
			if !strings.Contains(out, want) {
				t.Fatalf("agents missing %s:\n%s", want, out)
			}
		}
	})

	t.Run("broadcast", func(t *testing.T) {
		out := mustCLI(t, alice, "send", "@all", "all hands")
		for _, want := range []string{"bob@hosta", "carol@hostb"} {
			if !strings.Contains(out, want) {
				t.Fatalf("broadcast results missing %s:\n%s", want, out)
			}
		}
		if strings.Contains(out, "alice@hosta") {
			t.Fatalf("broadcast echoed to sender:\n%s", out)
		}
		for who, env := range map[string][]string{"bob": bob, "carol": carol} {
			if out := mustCLI(t, env, "recv"); !strings.Contains(out, "all hands") {
				t.Fatalf("%s missed broadcast: %q", who, out)
			}
		}
	})

	t.Run("cross-hub-ask", func(t *testing.T) {
		ch := make(chan string, 1)
		go func() {
			out, _ := cli(t, carol, "ask", "--timeout", "30", "alice@hosta", "ping?")
			ch <- out
		}()
		idRe := regexp.MustCompile(`(?m)^([0-9a-f]{16})  from carol@hostb`)
		var askID string
		deadline := time.Now().Add(10 * time.Second)
		for askID == "" && time.Now().Before(deadline) {
			out := mustCLI(t, alice, "asks")
			if m := idRe.FindStringSubmatch(out); m != nil {
				askID = m[1]
			} else {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if askID == "" {
			t.Fatal("alice never saw carol's ask")
		}
		mustCLI(t, alice, "answer", askID, "pong")
		if out := <-ch; strings.TrimSpace(out) != "pong" {
			t.Fatalf("cross-hub ask: %q", out)
		}
	})

	// ---- control across hubs: spawn/keys/read/kill on B, driven from A ----
	t.Run("cross-hub-control", func(t *testing.T) {
		out := mustCLI(t, a.env(), "spawn", "--host", "hostb", "--wait", "worker", "--", "cat")
		if !strings.Contains(out, "spawned worker@hostb") {
			t.Fatalf("spawn: %q", out)
		}
		mustCLI(t, a.env(), "keys", "--enter", "worker@hostb", "hello from hosta")
		screen := ""
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			screen = mustCLI(t, a.env(), "read", "worker@hostb")
			if strings.Contains(screen, "hello from hosta") {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !strings.Contains(screen, "hello from hosta") {
			t.Fatalf("keys never reached the pane:\n%s", screen)
		}

		// The spawned agent is registered and visible mesh-wide.
		if out := mustCLI(t, a.env(), "agents"); !strings.Contains(out, "worker@hostb") {
			t.Fatalf("spawned agent not in mesh listing:\n%s", out)
		}

		// New mail nudges the idle pane.
		mustCLI(t, alice, "send", "worker@hostb", "you have mail")
		deadline = time.Now().Add(5 * time.Second)
		nudged := false
		for !nudged && time.Now().Before(deadline) {
			screen = mustCLI(t, a.env(), "read", "worker@hostb")
			nudged = strings.Contains(screen, "hive recv")
			if !nudged {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if !nudged {
			t.Fatalf("idle agent never nudged:\n%s", screen)
		}
		if strings.Contains(screen, "you have mail") {
			t.Fatalf("nudge leaked the message body:\n%s", screen)
		}

		out = mustCLI(t, a.env(), "kill", "worker@hostb")
		if !strings.Contains(out, "killed worker@hostb") {
			t.Fatalf("kill: %q", out)
		}
		if out := mustCLI(t, a.env(), "agents"); strings.Contains(out, "worker@hostb") {
			t.Fatalf("killed agent still listed:\n%s", out)
		}
	})

	// ---- msg-only host: no control token in net.json ----
	t.Run("msg-only-host", func(t *testing.T) {
		c := startHub(t, "hostc")
		mustCLI(t, c.env(), "net", "join", "dev", "--hub", a.addr(), "--msg-token", a.msgTok)
		mustCLI(t, a.env(), "hosts", "add", "hostc", c.addr())
		dave := register(t, c, "dave")
		mustCLI(t, dave, "send", "alice@hosta", "from the cheap seats")
		if out := mustCLI(t, alice, "recv"); !strings.Contains(out, "from the cheap seats") {
			t.Fatalf("msg-only host send failed: %q", out)
		}
		// Without the control token, control ops fail client-side.
		if _, err := cli(t, c.env(), "spawn", "--host", "hosta", "evil", "--", "cat"); err == nil {
			t.Fatal("spawn from msg-only host should fail")
		}
	})
}

// envVal extracts one KEY=value from an env slice (last wins).
func envVal(t *testing.T, env []string, key string) string {
	t.Helper()
	val := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			val = v
		}
	}
	if val == "" {
		t.Fatalf("%s not in env", key)
	}
	return val
}
