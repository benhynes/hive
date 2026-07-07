// Package client is the HTTP client used by every hive CLI subcommand.
// Config resolution (env first, then local files):
//
//	HIVE_ADDR  hub to talk to        (default http://127.0.0.1:<config port>)
//	HIVE_NET   network name          (default: the sole local network)
//	HIVE_TOKEN bearer for msg ops    (default: tokens from ~/.hive/nets/<net>/net.json)
//	HIVE_CONTROL_TOKEN bearer for control ops (default: control token from net.json)
//	HIVE_AGENT our own agent id, informational
package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/proto"
)

type Client struct {
	Addr    string // local hub base, e.g. http://127.0.0.1:7777
	Net     string
	Token   string // msg-layer bearer (agent token or net token)
	Control string // control token if held
	Agent   string // our own id (name@host) if known
	hc      *http.Client

	self  string            // local hub's host name (lazy)
	hosts map[string]string // local hub's hosts list (lazy)
}

// Resolve builds a client from env, flags, and local config.
func Resolve(netFlag string) (*Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	c := &Client{
		Addr:  os.Getenv("HIVE_ADDR"),
		Net:   netFlag,
		Agent: os.Getenv("HIVE_AGENT"),
		hc:    &http.Client{Timeout: 35 * time.Second},
	}
	if c.Addr == "" {
		// Mirror hSpawn's HIVE_ADDR logic: a daemon bound to a specific
		// address (e.g. a tailnet IP) doesn't listen on loopback.
		host := cfg.Bind
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		c.Addr = "http://" + net.JoinHostPort(host, strconv.Itoa(cfg.Port))
	}
	if c.Net == "" {
		c.Net = os.Getenv("HIVE_NET")
	}
	if c.Net == "" {
		nets, _ := config.ListNets()
		switch len(nets) {
		case 1:
			c.Net = nets[0]
		case 0:
			return nil, fmt.Errorf("no networks configured — run: hive net create <name>")
		default:
			return nil, fmt.Errorf("multiple networks (%s) — pass --net or set HIVE_NET", strings.Join(nets, ", "))
		}
	}
	nc, haveNet := config.NetConfig{}, false
	if n, err := config.LoadNet(c.Net); err == nil {
		nc, haveNet = n, true
	}
	c.Token = os.Getenv("HIVE_TOKEN")
	c.Control = os.Getenv("HIVE_CONTROL_TOKEN")
	if (c.Token == "" || c.Control == "") && haveNet {
		if c.Control == "" {
			c.Control = nc.ControlToken
		}
		if c.Token == "" {
			c.Token = nc.ControlToken
			if c.Token == "" {
				c.Token = nc.MsgToken
			}
		}
	}
	if c.Token == "" {
		return nil, fmt.Errorf("no token: set HIVE_TOKEN or join network %q on this host", c.Net)
	}
	// Seed identity + routing from local config so Self()/hubFor() skip the
	// GET /hosts round-trip that nearly every command otherwise pays. Only
	// when talking to the local hub (HIVE_ADDR unset), whose self name is
	// this host's config name; hubFor still refreshes live on a hosts miss,
	// so a just-added peer resolves.
	if os.Getenv("HIVE_ADDR") == "" {
		c.self = cfg.HostName
		if haveNet {
			c.hosts = nc.Hosts
		}
	}
	return c, nil
}

func (c *Client) controlToken() (string, error) {
	if c.Control != "" {
		return c.Control, nil
	}
	return "", fmt.Errorf("control token required (set HIVE_CONTROL_TOKEN or hold it in net.json)")
}

func (c *Client) do(method, base, path, token string, in, out any) error {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if c.Agent != "" {
		if name, _, err := proto.SplitAgent(c.Agent); err == nil {
			req.Header.Set("X-Hive-Actor", name)
		}
	}
	resp, err := c.hc.Do(req)
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

func (c *Client) np(rest string) string { return "/v1/nets/" + c.Net + rest }

// ---- discovery / hosts ----

type HostsResp struct {
	Self  string            `json:"self"`
	Hosts map[string]string `json:"hosts"`
}

func (c *Client) Hosts() (HostsResp, error) {
	var out HostsResp
	err := c.do("GET", c.Addr, c.np("/hosts"), c.Token, nil, &out)
	if err == nil {
		c.self, c.hosts = out.Self, out.Hosts
	}
	return out, err
}

func (c *Client) HostsMod(op, name, addr string) (HostsResp, error) {
	tok, err := c.controlToken()
	if err != nil {
		return HostsResp{}, err
	}
	var out HostsResp
	err = c.do("POST", c.Addr, c.np("/hosts"), tok,
		map[string]string{"op": op, "name": name, "addr": addr}, &out)
	return out, err
}

// Self returns the local hub's host name.
func (c *Client) Self() (string, error) {
	if c.self == "" {
		if _, err := c.Hosts(); err != nil {
			return "", err
		}
	}
	return c.self, nil
}

// ExpandAgent turns "name" into "name@<localhost>"; full ids pass through.
func (c *Client) ExpandAgent(a string) (string, error) {
	if strings.Contains(a, "@") || a == proto.Broadcast {
		return a, nil
	}
	self, err := c.Self()
	if err != nil {
		return "", err
	}
	return a + "@" + self, nil
}

// hubFor returns the base URL of the hub owning the given host name.
func (c *Client) hubFor(host string) (string, error) {
	self, err := c.Self()
	if err != nil {
		return "", err
	}
	if host == self {
		return c.Addr, nil
	}
	addr, ok := c.hosts[host]
	if !ok {
		// Not in the (possibly seeded, possibly stale) local map — ask the
		// hub live, so a peer added since this process started resolves.
		fresh, err := c.Hosts()
		if err != nil {
			return "", err
		}
		addr, ok = fresh.Hosts[host]
	}
	if !ok {
		return "", fmt.Errorf("unknown host %q — add it with: hive hosts add %s <addr:port>", host, host)
	}
	return "http://" + addr, nil
}

type AgentInfo struct {
	Agent        string `json:"agent"`
	Alive        bool   `json:"alive"`
	Controllable bool   `json:"controllable"`
	Spawned      bool   `json:"spawned,omitempty"`
	Registered   int64  `json:"registered"`
}

type AgentsResp struct {
	Agents      []AgentInfo       `json:"agents"`
	Unreachable map[string]string `json:"unreachable,omitempty"`
}

func (c *Client) Agents(localOnly bool) (AgentsResp, error) {
	var out AgentsResp
	p := c.np("/agents")
	if localOnly {
		p += "?local=1"
	}
	err := c.do("GET", c.Addr, p, c.Token, nil, &out)
	return out, err
}

// ---- registration ----

type RegisterResp struct {
	Agent string `json:"agent"`
	Token string `json:"token"`
}

func (c *Client) Register(name, pane string, pid int) (RegisterResp, error) {
	var out RegisterResp
	err := c.do("POST", c.Addr, c.np("/register"), c.Token,
		map[string]any{"name": name, "pane": pane, "pid": pid}, &out)
	return out, err
}

func (c *Client) Deregister(name string) error {
	// Deregistering someone else needs the control layer; an agent's own
	// msg token only covers self-deregistration (empty name).
	tok := c.Token
	if name != "" && c.Control != "" {
		tok = c.Control
	}
	return c.do("POST", c.Addr, c.np("/deregister"), tok, map[string]string{"name": name}, nil)
}

// ---- messaging ----

type SendResp struct {
	ID      string            `json:"id"`
	Results map[string]string `json:"results"`
}

func (c *Client) Send(to, kind, body, corrID string) (SendResp, error) {
	var out SendResp
	err := c.do("POST", c.Addr, c.np("/send"), c.Token,
		map[string]string{"to": to, "kind": kind, "body": body, "corr_id": corrID}, &out)
	return out, err
}

type Rec struct {
	Seq int64          `json:"seq"`
	Env proto.Envelope `json:"env"`
}

type ReadResult struct {
	Msgs    []Rec `json:"msgs"`
	Cursor  int64 `json:"cursor"`
	Latest  int64 `json:"latest"`
	Skipped int64 `json:"skipped"`
}

// Inbox reads the caller's inbox. after < 0 means "from the stored
// cursor". wait seconds long-polls server-side (≤25s per request).
func (c *Client) Inbox(after int64, wait, max int, agent string) (ReadResult, error) {
	q := fmt.Sprintf("?wait=%d&max=%d", wait, max)
	if after >= 0 {
		q += "&after=" + strconv.FormatInt(after, 10)
	}
	if agent != "" {
		q += "&agent=" + agent
	}
	var out ReadResult
	tok := c.Token
	if agent != "" && c.Control != "" {
		tok = c.Control
	}
	err := c.do("GET", c.Addr, c.np("/inbox")+q, tok, nil, &out)
	return out, err
}

func (c *Client) InboxStat() (ReadResult, error) {
	var out ReadResult
	err := c.do("GET", c.Addr, c.np("/inbox?stat=1"), c.Token, nil, &out)
	return out, err
}

func (c *Client) Ack(seq int64) error {
	return c.do("POST", c.Addr, c.np("/ack"), c.Token, map[string]int64{"seq": seq}, nil)
}

// Ask sends a question and blocks until the answer, timeout, or an
// undeliverable send. Pure client-side composition: the answer is
// matched in our inbox by corr_id on a private read position, so
// regular mail is never consumed.
func (c *Client) Ask(to, body string, timeout time.Duration) (answer string, status string, err error) {
	stat, err := c.InboxStat()
	if err != nil {
		return "", "", fmt.Errorf("ask needs a registered agent identity: %w", err)
	}
	sent, err := c.Send(to, proto.KindAsk, body, "")
	if err != nil {
		return "", "", err
	}
	if res := sent.Results[to]; res != "delivered" {
		return "", "undeliverable", fmt.Errorf("%s", res)
	}
	after := stat.Latest
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		wait := int(time.Until(deadline).Seconds()) + 1
		if wait > 25 {
			wait = 25
		}
		res, err := c.Inbox(after, wait, 500, "")
		if err != nil {
			return "", "", err
		}
		for _, m := range res.Msgs {
			if m.Seq > after {
				after = m.Seq
			}
			if m.Env.Kind == proto.KindAnswer && m.Env.CorrID == sent.ID {
				return m.Env.Body, "answered", nil
			}
		}
	}
	return "", "timeout", nil
}

// scanInbox pages through the caller's whole retained window (the
// server caps one read at 500; the window holds up to 1000) without
// touching the cursor.
func (c *Client) scanInbox() ([]Rec, error) {
	var out []Rec
	after := int64(0)
	for {
		res, err := c.Inbox(after, 0, 500, "")
		if err != nil {
			return nil, err
		}
		out = append(out, res.Msgs...)
		if len(res.Msgs) == 0 {
			return out, nil
		}
		last := res.Msgs[len(res.Msgs)-1].Seq
		if last <= after {
			return out, nil
		}
		after = last
	}
}

// Asks lists ask-kind messages currently retained in our inbox.
func (c *Client) Asks() ([]Rec, error) {
	msgs, err := c.scanInbox()
	if err != nil {
		return nil, err
	}
	var out []Rec
	for _, m := range msgs {
		if m.Env.Kind == proto.KindAsk {
			out = append(out, m)
		}
	}
	return out, nil
}

// Answer replies to an ask by envelope id.
func (c *Client) Answer(askID, body string) (SendResp, error) {
	msgs, err := c.scanInbox()
	if err != nil {
		return SendResp{}, err
	}
	for _, m := range msgs {
		if m.Env.ID == askID && m.Env.Kind == proto.KindAsk {
			return c.Send(m.Env.From, proto.KindAnswer, body, askID)
		}
	}
	return SendResp{}, fmt.Errorf("no ask %q in inbox", askID)
}

// ---- control (direct to the target host's hub) ----

type SpawnResp struct {
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Pane    string `json:"pane"`
	Ready   bool   `json:"ready"`
	Window  string `json:"window,omitempty"`
}

func (c *Client) Spawn(host, name string, cmd []string, cwd string, grantControl, waitReady, headed bool) (SpawnResp, error) {
	tok, err := c.controlToken()
	if err != nil {
		return SpawnResp{}, err
	}
	if host == "" {
		if host, err = c.Self(); err != nil {
			return SpawnResp{}, err
		}
	}
	base, err := c.hubFor(host)
	if err != nil {
		return SpawnResp{}, err
	}
	var out SpawnResp
	err = c.do("POST", base, c.np("/spawn"), tok, map[string]any{
		"name": name, "cmd": cmd, "cwd": cwd,
		"grant_control": grantControl, "wait_ready": waitReady, "headed": headed,
	}, &out)
	return out, err
}

// controlTarget resolves an agent id to (hubBase, fullID).
func (c *Client) controlTarget(agent string) (string, string, string, error) {
	tok, err := c.controlToken()
	if err != nil {
		return "", "", "", err
	}
	full, err := c.ExpandAgent(agent)
	if err != nil {
		return "", "", "", err
	}
	_, host, err := proto.SplitAgent(full)
	if err != nil {
		return "", "", "", err
	}
	base, err := c.hubFor(host)
	if err != nil {
		return "", "", "", err
	}
	return base, full, tok, nil
}

func (c *Client) Keys(agent, text string, enter bool) error {
	base, full, tok, err := c.controlTarget(agent)
	if err != nil {
		return err
	}
	return c.do("POST", base, c.np("/keys"), tok,
		map[string]any{"agent": full, "text": text, "enter": enter}, nil)
}

func (c *Client) Read(agent string, lines int) (string, error) {
	base, full, tok, err := c.controlTarget(agent)
	if err != nil {
		return "", err
	}
	var out struct {
		Screen string `json:"screen"`
	}
	err = c.do("GET", base, c.np(fmt.Sprintf("/read?agent=%s&lines=%d", full, lines)), tok, nil, &out)
	return out.Screen, err
}

func (c *Client) Kill(agent string) (killed bool, err error) {
	base, full, tok, err := c.controlTarget(agent)
	if err != nil {
		return false, err
	}
	var out struct {
		Killed bool `json:"killed"`
	}
	err = c.do("POST", base, c.np("/kill"), tok, map[string]string{"agent": full}, &out)
	return out.Killed, err
}
