package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/benhynes/hive/internal/client"
	"github.com/benhynes/hive/internal/proto"
)

// Tools builds the tool set for c. CONTROL tools are omitted entirely unless
// the client holds the control credential — an MSG-only agent should never
// see hive_spawn in tools/list, because a listed-then-refused tool just
// invites the model to plan around a capability it does not have.
func Tools(c *client.Client) []Tool {
	t := []Tool{
		agentsTool(c),
		sendTool(c),
		recvTool(c),
		askTool(c),
		asksTool(c),
		answerTool(c),
	}
	if c.HasControl() {
		t = append(t, spawnTool(c), keysTool(c), readTool(c), killTool(c))
	}
	return t
}

// schema is a small helper for writing JSON Schema without a struct zoo.
func schema(s string) json.RawMessage { return json.RawMessage(s) }

// decode unmarshals tool arguments. MCP clients may omit `arguments`
// entirely for a no-arg tool, so an empty payload is a valid empty object.
func decode(args json.RawMessage, v any) error {
	if len(args) == 0 || string(args) == "null" {
		return nil
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}

// jsonOut renders a value as indented JSON for the model to read.
func jsonOut(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func agentsTool(c *client.Client) Tool {
	return Tool{
		Name: "hive_agents",
		Description: "List the agents on the hive mesh and whether each is alive. " +
			"Use this to discover who you can message before sending.",
		Schema: schema(`{
  "type": "object",
  "properties": {
    "local_only": {"type": "boolean", "description": "Only agents on this host (skips querying peer hubs)."}
  }
}`),
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				LocalOnly bool `json:"local_only"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			res, err := c.Agents(a.LocalOnly)
			if err != nil {
				return "", err
			}
			return jsonOut(res)
		},
	}
}

func sendTool(c *client.Client) Tool {
	return Tool{
		Name: "hive_send",
		Description: "Send a message to another agent. `to` is name@host, or a bare name for an " +
			"agent on your own host, or @all to broadcast to everyone but you. Delivery is durable: " +
			"the message waits in the recipient's inbox until they read it. Bodies are capped at 8 KiB — " +
			"point at files or URLs instead of pasting large blobs.",
		Schema: schema(`{
  "type": "object",
  "properties": {
    "to":   {"type": "string", "description": "name@host, a bare name on your own host, or @all."},
    "body": {"type": "string", "description": "Message text (max 8192 bytes)."}
  },
  "required": ["to", "body"]
}`),
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				To   string `json:"to"`
				Body string `json:"body"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.To == "" || a.Body == "" {
				return "", fmt.Errorf("`to` and `body` are required")
			}
			to, err := c.ExpandAgent(a.To)
			if err != nil {
				return "", err
			}
			res, err := c.Send(to, proto.KindMsg, a.Body, "")
			if err != nil {
				return "", err
			}
			// A single-target send that was not delivered is a failure the model
			// must see, not a success with a footnote.
			if to != proto.Broadcast {
				if st := res.Results[to]; st != "delivered" {
					return "", fmt.Errorf("%s: %s", to, st)
				}
			}
			return jsonOut(res)
		},
	}
}

func recvTool(c *client.Client) Tool {
	return Tool{
		Name: "hive_recv",
		Description: "Read new messages from your inbox. By default this acknowledges what it returns, " +
			"so the next call returns only newer messages. Set `wait` to block until mail arrives " +
			"(useful as a work loop). Asks are returned with an `ask_id` — answer each one with hive_answer. " +
			"Messages may very rarely arrive twice; treat them idempotently.",
		Schema: schema(`{
  "type": "object",
  "properties": {
    "wait": {"type": "integer", "description": "Seconds to block waiting for at least one message (0 = return immediately, max 25)."},
    "max":  {"type": "integer", "description": "Maximum messages to return (default 100)."},
    "peek": {"type": "boolean", "description": "Read without acknowledging, so the same messages are returned again next call."}
  }
}`),
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Wait int  `json:"wait"`
				Max  int  `json:"max"`
				Peek bool `json:"peek"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.Max <= 0 {
				a.Max = 100
			}
			if a.Wait > 25 {
				a.Wait = 25 // one server-side long-poll; the model can call again
			}
			res, err := c.Inbox(-1, a.Wait, a.Max, "")
			if err != nil {
				return "", err
			}

			type msg struct {
				Seq   int64  `json:"seq"`
				From  string `json:"from"`
				Kind  string `json:"kind"`
				Body  string `json:"body"`
				At    string `json:"at"`
				AskID string `json:"ask_id,omitempty"`
			}
			out := struct {
				Messages []msg  `json:"messages"`
				Warning  string `json:"warning,omitempty"`
			}{Messages: []msg{}}

			var top int64
			for _, m := range res.Msgs {
				e := msg{
					Seq:  m.Seq,
					From: m.Env.From,
					Kind: m.Env.Kind,
					Body: m.Env.Body,
					At:   time.UnixMilli(m.Env.TS).Format(time.RFC3339),
				}
				switch m.Env.Kind {
				case proto.KindAsk:
					e.AskID = m.Env.ID // what hive_answer wants
				case proto.KindAnswer:
					e.AskID = m.Env.CorrID // the ask this answers
				}
				out.Messages = append(out.Messages, e)
				if m.Seq > top {
					top = m.Seq
				}
			}
			if res.Skipped > 0 {
				out.Warning = fmt.Sprintf("%d message(s) were dropped unread (inbox overflowed its 1000-message window)", res.Skipped)
			}
			if top > 0 && !a.Peek {
				if err := c.Ack(top); err != nil {
					return "", err
				}
			}
			return jsonOut(out)
		},
	}
}

func askTool(c *client.Client) Tool {
	return Tool{
		Name: "hive_ask",
		Description: "Ask another agent a question and block until they answer. Returns the answer text. " +
			"Use this instead of hive_send when you cannot proceed without the reply. The target must be a " +
			"single agent — you cannot ask @all, because the answers would race.",
		Schema: schema(`{
  "type": "object",
  "properties": {
    "to":       {"type": "string", "description": "name@host, or a bare name on your own host."},
    "question": {"type": "string", "description": "The question (max 8192 bytes)."},
    "timeout":  {"type": "integer", "description": "Seconds to wait for the answer (default 60)."}
  },
  "required": ["to", "question"]
}`),
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				To       string `json:"to"`
				Question string `json:"question"`
				Timeout  int    `json:"timeout"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.To == "" || a.Question == "" {
				return "", fmt.Errorf("`to` and `question` are required")
			}
			if a.To == proto.Broadcast {
				return "", fmt.Errorf("ask needs a single target (broadcast answers would race)")
			}
			if a.Timeout <= 0 {
				a.Timeout = 60
			}
			to, err := c.ExpandAgent(a.To)
			if err != nil {
				return "", err
			}
			answer, status, err := c.Ask(to, a.Question, time.Duration(a.Timeout)*time.Second)
			switch status {
			case "answered":
				return answer, nil
			case "undeliverable":
				return "", fmt.Errorf("%s undeliverable: %v", to, err)
			case "timeout":
				return "", fmt.Errorf("no answer from %s within %ds — the ask was delivered, so the answer may still arrive later via hive_recv", to, a.Timeout)
			default:
				return "", err
			}
		},
	}
}

func asksTool(c *client.Client) Tool {
	return Tool{
		Name: "hive_asks",
		Description: "List questions other agents are waiting on you to answer. Someone is blocked on each " +
			"of these — answer them promptly with hive_answer. Recently answered asks are included too; " +
			"answer each ask_id only once.",
		Schema: schema(`{"type": "object", "properties": {}}`),
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			asks, err := c.Asks()
			if err != nil {
				return "", err
			}
			type ask struct {
				AskID string `json:"ask_id"`
				From  string `json:"from"`
				Body  string `json:"body"`
				At    string `json:"at"`
			}
			out := []ask{}
			for _, m := range asks {
				out = append(out, ask{
					AskID: m.Env.ID,
					From:  m.Env.From,
					Body:  m.Env.Body,
					At:    time.UnixMilli(m.Env.TS).Format(time.RFC3339),
				})
			}
			return jsonOut(out)
		},
	}
}

func answerTool(c *client.Client) Tool {
	return Tool{
		Name: "hive_answer",
		Description: "Answer a question another agent asked you. Get the ask_id from hive_recv or hive_asks. " +
			"The asking agent is blocked until this lands.",
		Schema: schema(`{
  "type": "object",
  "properties": {
    "ask_id": {"type": "string", "description": "The ask_id from hive_recv or hive_asks."},
    "body":   {"type": "string", "description": "Your answer (max 8192 bytes)."}
  },
  "required": ["ask_id", "body"]
}`),
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				AskID string `json:"ask_id"`
				Body  string `json:"body"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.AskID == "" || a.Body == "" {
				return "", fmt.Errorf("`ask_id` and `body` are required")
			}
			res, err := c.Answer(a.AskID, a.Body)
			if err != nil {
				return "", err
			}
			for to, st := range res.Results {
				if st != "delivered" {
					return "", fmt.Errorf("%s: %s", to, st)
				}
			}
			return jsonOut(res)
		},
	}
}

// ---- CONTROL layer ----------------------------------------------------
//
// Only registered when the agent holds HIVE_CONTROL_TOKEN. These do exactly
// what a human at the keyboard could do, and every one is audit-logged on the
// host where it happens.

func spawnTool(c *client.Client) Tool {
	return Tool{
		Name: "hive_spawn",
		Description: "Spawn a new agent into the mesh as a tmux session on a host. `cmd` is exec'd without " +
			"shell interpolation (e.g. [\"claude\"]). The new agent gets its hive identity injected, so it can " +
			"message you back immediately.",
		Schema: schema(`{
  "type": "object",
  "properties": {
    "name":          {"type": "string", "description": "Agent name: lowercase [a-z0-9_-], unique on its host."},
    "cmd":           {"type": "array", "items": {"type": "string"}, "description": "Command to run, argv style. Not shell-interpreted."},
    "host":          {"type": "string", "description": "Host to spawn on (default: your own host)."},
    "cwd":           {"type": "string", "description": "Working directory for the new agent. Required for context/MCP provisioning to run."},
    "profile":       {"type": "string", "description": "Spawn profile name: seeds context files + registers MCP servers (incl. hive) in the agent's cwd."},
    "grant_control": {"type": "boolean", "description": "Give the new agent the CONTROL credential too. It will be able to spawn and control other agents."},
    "wait_ready":    {"type": "boolean", "description": "Wait until the agent's pane stops changing (up to 15s) before returning."},
    "headed":        {"type": "boolean", "description": "Also open a visible terminal window on the target host so a human can watch."},
    "persist":       {"type": "boolean", "description": "Respawn this agent automatically after a reboot."}
  },
  "required": ["name", "cmd"]
}`),
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Name         string   `json:"name"`
				Cmd          []string `json:"cmd"`
				Host         string   `json:"host"`
				Cwd          string   `json:"cwd"`
				Profile      string   `json:"profile"`
				GrantControl bool     `json:"grant_control"`
				WaitReady    bool     `json:"wait_ready"`
				Headed       bool     `json:"headed"`
				Persist      bool     `json:"persist"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.Name == "" || len(a.Cmd) == 0 {
				return "", fmt.Errorf("`name` and a non-empty `cmd` are required")
			}
			res, err := c.Spawn(a.Host, a.Name, a.Cmd, a.Cwd, a.Profile, a.GrantControl, a.WaitReady, a.Headed, a.Persist)
			if err != nil {
				return "", err
			}
			return jsonOut(res)
		},
	}
}

func keysTool(c *client.Client) Tool {
	return Tool{
		Name: "hive_keys",
		Description: "Type text into another agent's terminal, as if at its keyboard. Prefer hive_send or " +
			"hive_ask when the target is hive-aware: messages queue durably, whereas keystrokes race with " +
			"whatever the agent is doing. After typing, give the TUI a moment, then hive_read to see the effect.",
		Schema: schema(`{
  "type": "object",
  "properties": {
    "agent": {"type": "string", "description": "Target agent: name@host, or a bare name on your own host."},
    "text":  {"type": "string", "description": "Text to type. Multi-line text is delivered as one bracketed paste."},
    "enter": {"type": "boolean", "description": "Press Enter afterwards (i.e. actually submit it)."}
  },
  "required": ["agent", "text"]
}`),
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Agent string `json:"agent"`
				Text  string `json:"text"`
				Enter bool   `json:"enter"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.Agent == "" || a.Text == "" {
				return "", fmt.Errorf("`agent` and `text` are required")
			}
			agent, err := c.ExpandAgent(a.Agent)
			if err != nil {
				return "", err
			}
			if err := c.Keys(agent, a.Text, a.Enter); err != nil {
				return "", err
			}
			return "typed into " + agent, nil
		},
	}
}

func readTool(c *client.Client) Tool {
	return Tool{
		Name:        "hive_read",
		Description: "Read another agent's terminal screen as text. Use this to see what an agent is doing or what it last printed.",
		Schema: schema(`{
  "type": "object",
  "properties": {
    "agent": {"type": "string", "description": "Target agent: name@host, or a bare name on your own host."},
    "lines": {"type": "integer", "description": "Lines of scrollback to include above the visible screen."}
  },
  "required": ["agent"]
}`),
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Agent string `json:"agent"`
				Lines int    `json:"lines"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.Agent == "" {
				return "", fmt.Errorf("`agent` is required")
			}
			agent, err := c.ExpandAgent(a.Agent)
			if err != nil {
				return "", err
			}
			screen, err := c.Read(agent, a.Lines)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(screen) == "" {
				return "(blank screen)", nil
			}
			return screen, nil
		},
	}
}

func killTool(c *client.Client) Tool {
	return Tool{
		Name:        "hive_kill",
		Description: "Kill an agent's tmux session and remove it from the mesh. This destroys its running work — prefer messaging it to wind down if it may have unsaved state.",
		Schema: schema(`{
  "type": "object",
  "properties": {
    "agent":  {"type": "string", "description": "Target agent: name@host, or a bare name on your own host."},
    "forget": {"type": "boolean", "description": "Also drop its persistent-session declaration, so it is not respawned after a reboot."}
  },
  "required": ["agent"]
}`),
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Agent  string `json:"agent"`
				Forget bool   `json:"forget"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.Agent == "" {
				return "", fmt.Errorf("`agent` is required")
			}
			agent, err := c.ExpandAgent(a.Agent)
			if err != nil {
				return "", err
			}
			killed, err := c.Kill(agent, a.Forget)
			if err != nil {
				return "", err
			}
			if killed {
				return "killed and deregistered " + agent, nil
			}
			return "deregistered " + agent + " (no live session to kill)", nil
		},
	}
}

// Names returns the tool names in listing order (used by `hive mcp --list`).
func Names(tools []Tool) []string {
	n := make([]string, 0, len(tools))
	for _, t := range tools {
		n = append(n, t.Name)
	}
	sort.Strings(n)
	return n
}
