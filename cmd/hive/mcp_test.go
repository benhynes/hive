// MCP end-to-end: drive `hive mcp` exactly as an MCP client would — a
// subprocess spoken to in newline-delimited JSON-RPC over stdin/stdout —
// against a real daemon, and check that messages actually cross the mesh.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/benhynes/hive/internal/client"
)

// mcpSession is a live `hive mcp` subprocess plus the pipes to talk to it.
type mcpSession struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Scanner
	cmd *exec.Cmd
	id  int

	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error
}

func startMCP(t *testing.T, env []string) *mcpSession {
	return startMCPArgs(t, env)
}

func startMCPArgs(t *testing.T, env []string, args ...string) *mcpSession {
	t.Helper()
	cmd := exec.Command(hiveBin(t), append([]string{"mcp"}, args...)...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	s := &mcpSession{t: t, in: stdin, out: sc, cmd: cmd, waitDone: make(chan struct{})}
	t.Cleanup(func() {
		_ = stdin.Close()
		if _, ok := s.wait(3 * time.Second); !ok {
			_ = cmd.Process.Kill()
			_, _ = s.wait(3 * time.Second)
		}
	})

	// Every MCP session opens with the handshake.
	res := s.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
		"capabilities":    map[string]any{},
	})
	if got := res["protocolVersion"]; got != "2025-06-18" {
		t.Fatalf("initialize: protocolVersion = %v, want the version we asked for", got)
	}
	s.notify("notifications/initialized")
	return s
}

func (s *mcpSession) wait(timeout time.Duration) (error, bool) {
	s.waitOnce.Do(func() {
		go func() {
			s.waitErr = s.cmd.Wait()
			close(s.waitDone)
		}()
	})
	select {
	case <-s.waitDone:
		return s.waitErr, true
	case <-time.After(timeout):
		return nil, false
	}
}

// rpc sends a request and returns its result, failing on a JSON-RPC error.
func (s *mcpSession) rpc(method string, params any) map[string]any {
	s.t.Helper()
	s.id++
	req := map[string]any{"jsonrpc": "2.0", "id": s.id, "method": method}
	if params != nil {
		req["params"] = params
	}
	line, err := json.Marshal(req)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.in.Write(append(line, '\n')); err != nil {
		s.t.Fatalf("write %s: %v", method, err)
	}
	if !s.out.Scan() {
		s.t.Fatalf("%s: no response (server exited?): %v", method, s.out.Err())
	}
	var resp struct {
		ID     int                       `json:"id"`
		Result map[string]any            `json:"result"`
		Error  *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(s.out.Bytes(), &resp); err != nil {
		s.t.Fatalf("%s: bad response %q: %v", method, s.out.Text(), err)
	}
	if resp.Error != nil {
		s.t.Fatalf("%s: rpc error: %s", method, resp.Error.Message)
	}
	if resp.ID != s.id {
		s.t.Fatalf("%s: response id = %d, want %d", method, resp.ID, s.id)
	}
	return resp.Result
}

// notify sends a notification, which must draw no response at all.
func (s *mcpSession) notify(method string) {
	s.t.Helper()
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if _, err := s.in.Write(append(line, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

// tools returns the tool names the server offers this agent.
func (s *mcpSession) tools() []string {
	s.t.Helper()
	res := s.rpc("tools/list", map[string]any{})
	list, _ := res["tools"].([]any)
	names := make([]string, 0, len(list))
	for _, it := range list {
		m, _ := it.(map[string]any)
		name, _ := m["name"].(string)
		// A tool with no schema is unusable by a model — catch that here.
		if m["inputSchema"] == nil {
			s.t.Errorf("tool %s has no inputSchema", name)
		}
		names = append(names, name)
	}
	return names
}

// call invokes a tool and returns its text content plus the isError flag.
func (s *mcpSession) call(name string, args map[string]any) (string, bool) {
	s.t.Helper()
	res := s.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	isErr, _ := res["isError"].(bool)
	content, _ := res["content"].([]any)
	var sb strings.Builder
	for _, c := range content {
		m, _ := c.(map[string]any)
		if txt, ok := m["text"].(string); ok {
			sb.WriteString(txt)
		}
	}
	return sb.String(), isErr
}

func (s *mcpSession) mustCall(name string, args map[string]any) string {
	s.t.Helper()
	out, isErr := s.call(name, args)
	if isErr {
		s.t.Fatalf("%s: tool error: %s", name, out)
	}
	return out
}

func hasTool(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestInjectedMCPIdentityIgnoresConfiguredName(t *testing.T) {
	c := &client.Client{Agent: "injected@host"}
	if err := validateMCPName(c, "Not/A/Valid/Hive/Name"); err != nil {
		t.Fatalf("injected identity was overridden by configured name validation: %v", err)
	}
	if err := validateMCPName(&client.Client{}, "Not/A/Valid/Hive/Name"); err == nil {
		t.Fatal("automatic registration accepted an invalid configured name")
	}
}

func TestMCPInitializesWhileEnrollmentIsDeferred(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and drives the MCP subprocess")
	}
	// Nothing is listening on this freshly released port. Initialization and
	// tool discovery must still work: enrollment is a recoverable tool-time
	// dependency, not a prerequisite for the MCP protocol handshake.
	addr := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	env := append(os.Environ(),
		"HIVE_HOME="+t.TempDir(), "HIVE_ADDR="+addr, "HIVE_NET=dev",
		"HIVE_TOKEN="+strings.Repeat("a", 64), "HIVE_AGENT=",
		"HIVE_CONTROL_TOKEN=", "HIVE_CONTROL_HOST=", "TMUX_PANE=",
	)
	s := startMCPArgs(t, env, "--name", "waiting")
	if names := s.tools(); !hasTool(names, "hive_agents") || !hasTool(names, "hive_send") {
		t.Fatalf("deferred MCP session did not advertise message tools: %v", names)
	}
	if err := s.in.Close(); err != nil {
		t.Fatal(err)
	}
	if err, ok := s.wait(5 * time.Second); !ok || err != nil {
		t.Fatalf("deferred MCP shutdown: ok=%v err=%v", ok, err)
	}
}

func requireMCPAgentState(t *testing.T, raw, agent string, wantAlive, wantEphemeral bool) {
	t.Helper()
	var roster client.AgentsResp
	if err := json.Unmarshal([]byte(raw), &roster); err != nil {
		t.Fatalf("decode agents: %v\n%s", err, raw)
	}
	for _, got := range roster.Agents {
		if got.Agent == agent {
			if got.Alive != wantAlive || got.Ephemeral != wantEphemeral {
				t.Fatalf("agent %s state=%+v, want alive=%v ephemeral=%v", agent, got, wantAlive, wantEphemeral)
			}
			return
		}
	}
	t.Fatalf("agent %s missing from roster: %s", agent, raw)
}

func TestMCPEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: daemon + mcp subprocesses")
	}
	h := startHub(t, "mcphost")
	out := mustCLI(t, h.env(), "net", "create", "dev")
	tokRe := regexp.MustCompile(`(?m)^  (msg|control) token: +([0-9a-f]{64})$`)
	for _, m := range tokRe.FindAllStringSubmatch(out, -1) {
		if m[1] == "msg" {
			h.msgTok = m[2]
		} else {
			h.ctlTok = m[2]
		}
	}
	if h.msgTok == "" || h.ctlTok == "" {
		t.Fatalf("net create output missing tokens:\n%s", out)
	}

	aliceEnv := register(t, h, "alice")
	bobEnv := register(t, h, "bob")
	alice := startMCP(t, aliceEnv)
	bob := startMCP(t, bobEnv)
	// No pre-registration and no injected identity: this is the one-time global
	// MCP configuration path used by an agent launched normally, outside tmux.
	auto := startMCPArgs(t, h.env(
		"HIVE_ADDR=", "HIVE_NET=", "HIVE_AGENT=", "HIVE_TOKEN=",
		"HIVE_CONTROL_TOKEN=", "HIVE_CONTROL_HOST=",
	), "--name", "autobot")

	t.Run("mcp-auto-registers-with-msg-only-identity", func(t *testing.T) {
		names := auto.tools()
		if !hasTool(names, "hive_send") || !hasTool(names, "hive_recv") {
			t.Fatalf("auto-registered agent missing message tools: %v", names)
		}
		for _, never := range []string{"hive_spawn", "hive_keys", "hive_read", "hive_kill"} {
			if hasTool(names, never) {
				t.Errorf("auto-registered agent inherited on-disk control tool %s", never)
			}
		}
		directory := auto.mustCall("hive_agents", map[string]any{})
		var ownDirectory struct {
			Self   string `json:"self"`
			Agents []any  `json:"agents"`
		}
		if err := json.Unmarshal([]byte(directory), &ownDirectory); err != nil {
			t.Fatalf("auto agent directory is not JSON: %v\n%s", err, directory)
		}
		if ownDirectory.Self != "autobot@mcphost" {
			t.Fatalf("auto agent directory self = %q, want autobot@mcphost\n%s", ownDirectory.Self, directory)
		}
		if len(ownDirectory.Agents) == 0 {
			t.Fatalf("adding self dropped the agents roster:\n%s", directory)
		}
		listed := alice.mustCall("hive_agents", map[string]any{})
		if !strings.Contains(listed, "autobot@mcphost") {
			t.Fatalf("auto-registered agent was not discoverable:\n%s", listed)
		}
		alice.mustCall("hive_send", map[string]any{"to": "autobot", "body": "hello without tmux"})
		got := auto.mustCall("hive_recv", map[string]any{})
		if !strings.Contains(got, "hello without tmux") || !strings.Contains(got, "alice@mcphost") {
			t.Fatalf("auto-registered agent did not receive mail:\n%s", got)
		}
	})

	t.Run("name-collision-does-not-block-handshake-and-recovers", func(t *testing.T) {
		env := h.env(
			"HIVE_HOME="+t.TempDir(), "HIVE_ADDR="+h.url(), "HIVE_NET=dev",
			"HIVE_AGENT=", "HIVE_TOKEN="+h.msgTok,
			"HIVE_CONTROL_TOKEN=", "HIVE_CONTROL_HOST=",
		)
		owner := startMCPArgs(t, env, "--name", "retry-agent")
		owner.mustCall("hive_agents", map[string]any{}) // force enrollment

		waiting := startMCPArgs(t, env, "--name", "retry-agent")
		if names := waiting.tools(); !hasTool(names, "hive_agents") {
			t.Fatalf("colliding session failed MCP discovery: %v", names)
		}
		if err := owner.in.Close(); err != nil {
			t.Fatal(err)
		}
		if err, ok := owner.wait(5 * time.Second); !ok || err != nil {
			t.Fatalf("release colliding owner: ok=%v err=%v", ok, err)
		}
		var directory struct {
			Self string `json:"self"`
		}
		if err := json.Unmarshal([]byte(waiting.mustCall("hive_agents", map[string]any{})), &directory); err != nil {
			t.Fatal(err)
		}
		if directory.Self != "retry-agent@mcphost" {
			t.Fatalf("re-enrolled self = %q", directory.Self)
		}
	})

	t.Run("msg-agent-sees-only-msg-tools", func(t *testing.T) {
		names := alice.tools()
		for _, want := range []string{"hive_send", "hive_recv", "hive_ask", "hive_answer", "hive_asks", "hive_agents"} {
			if !hasTool(names, want) {
				t.Errorf("MSG agent is missing %s (got %v)", want, names)
			}
		}
		// The point of hiding these: a model must never plan around a
		// capability it does not hold.
		for _, never := range []string{"hive_spawn", "hive_keys", "hive_read", "hive_kill"} {
			if hasTool(names, never) {
				t.Errorf("MSG-only agent was offered control tool %s", never)
			}
		}
	})

	t.Run("control-agent-sees-control-tools", func(t *testing.T) {
		ctl := startMCP(t, append(append([]string{}, aliceEnv...), "HIVE_CONTROL_TOKEN="+h.ctlTok))
		names := ctl.tools()
		for _, want := range []string{"hive_spawn", "hive_keys", "hive_read", "hive_kill"} {
			if !hasTool(names, want) {
				t.Errorf("control agent is missing %s (got %v)", want, names)
			}
		}
	})

	t.Run("agents-discovery", func(t *testing.T) {
		out := alice.mustCall("hive_agents", map[string]any{})
		if !strings.Contains(out, "bob@mcphost") {
			t.Fatalf("hive_agents did not list bob:\n%s", out)
		}
	})

	t.Run("send-and-recv", func(t *testing.T) {
		alice.mustCall("hive_send", map[string]any{"to": "bob", "body": "the build is green"})

		out := bob.mustCall("hive_recv", map[string]any{})
		if !strings.Contains(out, "the build is green") || !strings.Contains(out, "alice@mcphost") {
			t.Fatalf("bob did not receive alice's message:\n%s", out)
		}
		// recv acks by default, so the same message must not come back.
		if out := bob.mustCall("hive_recv", map[string]any{}); strings.Contains(out, "the build is green") {
			t.Fatalf("hive_recv redelivered an acked message:\n%s", out)
		}
	})

	t.Run("send-to-unknown-agent-is-a-tool-error", func(t *testing.T) {
		// Must surface as isError content the model can read and adapt to,
		// not as a JSON-RPC protocol error that kills the call.
		out, isErr := alice.call("hive_send", map[string]any{"to": "nobody", "body": "hi"})
		if !isErr {
			t.Fatalf("send to a nonexistent agent reported success: %s", out)
		}
		if !strings.Contains(out, "undeliverable") {
			t.Fatalf("error text should say why: %q", out)
		}
	})

	t.Run("ask-and-answer", func(t *testing.T) {
		// alice blocks on the ask; bob answers it out of band.
		type askResult struct {
			text  string
			isErr bool
		}
		done := make(chan askResult, 1)
		go func() {
			text, isErr := alice.call("hive_ask", map[string]any{
				"to": "bob", "question": "which port does staging use?", "timeout": 30,
			})
			done <- askResult{text, isErr}
		}()

		// Wait for the ask to land in bob's queue, then answer it.
		var askID string
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) && askID == "" {
			var asks []struct {
				AskID string `json:"ask_id"`
				Body  string `json:"body"`
			}
			if err := json.Unmarshal([]byte(bob.mustCall("hive_asks", map[string]any{})), &asks); err != nil {
				t.Fatal(err)
			}
			for _, a := range asks {
				if strings.Contains(a.Body, "staging") {
					askID = a.AskID
				}
			}
			if askID == "" {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if askID == "" {
			t.Fatal("the ask never appeared in bob's hive_asks")
		}
		bob.mustCall("hive_answer", map[string]any{"ask_id": askID, "body": "port 8443"})

		select {
		case got := <-done:
			if got.isErr {
				t.Fatalf("hive_ask failed: %s", got.text)
			}
			// The answer text is the whole point — it must come back verbatim.
			if strings.TrimSpace(got.text) != "port 8443" {
				t.Fatalf("hive_ask returned %q, want %q", got.text, "port 8443")
			}
		case <-time.After(30 * time.Second):
			t.Fatal("hive_ask never returned after the answer was sent")
		}
	})

	t.Run("named-auto-identity-goes-offline-on-stdio-eof", func(t *testing.T) {
		if err := auto.in.Close(); err != nil {
			t.Fatal(err)
		}
		if err, ok := auto.wait(5 * time.Second); !ok {
			t.Fatal("hive mcp did not exit after stdin EOF")
		} else if err != nil {
			t.Fatalf("hive mcp EOF exit: %v", err)
		}
		agents := mustCLI(t, h.env(), "agents", "--local", "--json")
		requireMCPAgentState(t, agents, "autobot@mcphost", false, false)
	})

	t.Run("sigterm-unblocks-stdio-and-releases-named-identity", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows does not provide Unix SIGTERM semantics")
		}
		s := startMCPArgs(t, h.env(
			"HIVE_HOME="+t.TempDir(), "HIVE_ADDR="+h.url(), "HIVE_NET=dev",
			"HIVE_AGENT=", "HIVE_TOKEN="+h.msgTok,
			"HIVE_CONTROL_TOKEN=", "HIVE_CONTROL_HOST=",
		), "--name", "term-agent")
		s.mustCall("hive_agents", map[string]any{}) // make enrollment deterministic before shutdown
		if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if err, ok := s.wait(5 * time.Second); !ok {
			t.Fatal("hive mcp stayed blocked in stdin after SIGTERM")
		} else if err != nil {
			t.Fatalf("hive mcp SIGTERM exit: %v", err)
		}
		agents := mustCLI(t, h.env(), "agents", "--local", "--json")
		requireMCPAgentState(t, agents, "term-agent@mcphost", false, false)
	})

	t.Run("generated-auto-identity-is-disposable", func(t *testing.T) {
		s := startMCPArgs(t, h.env(
			"HIVE_HOME="+t.TempDir(), "HIVE_ADDR="+h.url(), "HIVE_NET=dev",
			"HIVE_AGENT=", "HIVE_TOKEN="+h.msgTok,
			"HIVE_CONTROL_TOKEN=", "HIVE_CONTROL_HOST=",
		))
		var directory struct {
			Self string `json:"self"`
		}
		if err := json.Unmarshal([]byte(s.mustCall("hive_agents", map[string]any{})), &directory); err != nil {
			t.Fatal(err)
		}
		if directory.Self == "" {
			t.Fatal("generated MCP identity did not report self")
		}
		oldSelf := directory.Self
		mustCLI(t, h.env(), "deregister", oldSelf)
		// Losing the record entirely (for example, after the crash-recovery
		// grace period) must not permanently poison a long-lived MCP session.
		// A 401 causes the gate to restore its bootstrap credential and mint a
		// fresh disposable identity before retrying the read-only tool call.
		if err := json.Unmarshal([]byte(s.mustCall("hive_agents", map[string]any{})), &directory); err != nil {
			t.Fatal(err)
		}
		if directory.Self == "" || directory.Self == oldSelf {
			t.Fatalf("generated identity did not recover after record loss: old=%q new=%q", oldSelf, directory.Self)
		}
		if err := s.in.Close(); err != nil {
			t.Fatal(err)
		}
		if err, ok := s.wait(5 * time.Second); !ok || err != nil {
			t.Fatalf("generated MCP shutdown: ok=%v err=%v", ok, err)
		}
		if agents := mustCLI(t, h.env(), "agents", "--local", "--json"); strings.Contains(agents, directory.Self) {
			t.Fatalf("generated identity remained after EOF: %s", agents)
		}
	})
}
