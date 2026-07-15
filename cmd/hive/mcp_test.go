// MCP end-to-end: drive `hive mcp` exactly as an MCP client would — a
// subprocess spoken to in newline-delimited JSON-RPC over stdin/stdout —
// against a real daemon, and check that messages actually cross the mesh.
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// mcpSession is a live `hive mcp` subprocess plus the pipes to talk to it.
type mcpSession struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Scanner
	cmd *exec.Cmd
	id  int
}

func startMCP(t *testing.T, env []string) *mcpSession {
	t.Helper()
	cmd := exec.Command(hiveBin(t), "mcp")
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
	t.Cleanup(func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	})
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	s := &mcpSession{t: t, in: stdin, out: sc, cmd: cmd}

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
}
