// Package hub implements the hive daemon: per-network registries,
// mailboxes, the two-layer auth model, tmux control, and hub-to-hub
// delivery.
//
// Auth model (two layers):
//   - The network MSG and CONTROL tokens are join/infrastructure
//     credentials, held by hubs and the human.
//   - Agents hold per-agent tokens minted at registration. They are
//     always MSG-layer and identity-bearing: `from` is stamped from the
//     token, so one agent cannot impersonate another.
//   - CONTROL is possession of the network control token. Spawning with
//     --grant-control injects it as HIVE_CONTROL_TOKEN.
package hub

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/proto"
	"github.com/benhynes/hive/internal/store"
	"github.com/benhynes/hive/internal/tmux"
)

// nudgeEvery is the minimum interval between nudge injections per agent.
const nudgeEvery = 30 * time.Second

// Hub is one host's daemon state.
type Hub struct {
	Cfg    config.Config
	mu     sync.Mutex
	nets   map[string]*network
	client *http.Client // hub->hub calls
}

type network struct {
	name string
	dir  string
	reg  *store.Registry

	// regMu serializes name claims (register/spawn): the aliveness
	// probes and tmux calls between the taken-check and the registry
	// Put would otherwise race, letting two claimants both succeed and
	// the loser silently revoke the winner's token.
	regMu sync.Mutex

	mu        sync.Mutex // guards cfg, inboxes, lastNudge
	cfg       config.NetConfig
	inboxes   map[string]*store.Inbox
	lastNudge map[string]time.Time

	auditMu sync.Mutex
	audit   *os.File
}

// New creates a hub for the given host config.
func New(cfg config.Config) *Hub {
	return &Hub{
		Cfg:    cfg,
		nets:   map[string]*network{},
		client: &http.Client{Timeout: 3500 * time.Millisecond},
	}
}

// net lazily loads a network from disk, so `hive net create/join` while
// the daemon runs needs no reload step.
func (h *Hub) net(name string) (*network, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n, ok := h.nets[name]; ok {
		return n, nil
	}
	if !proto.ValidName(name) {
		return nil, fmt.Errorf("bad network name")
	}
	cfg, err := config.LoadNet(name)
	if err != nil {
		return nil, err
	}
	dir := config.NetDir(name)
	reg, err := store.OpenRegistry(dir)
	if err != nil {
		return nil, err
	}
	audit, err := os.OpenFile(filepath.Join(dir, "audit.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	n := &network{
		name: name, dir: dir, reg: reg, cfg: cfg,
		inboxes:   map[string]*store.Inbox{},
		lastNudge: map[string]time.Time{},
		audit:     audit,
	}
	h.nets[name] = n
	return n, nil
}

func (n *network) inbox(agent string) (*store.Inbox, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ib, ok := n.inboxes[agent]; ok {
		return ib, nil
	}
	ib, err := store.OpenInbox(n.dir, agent)
	if err != nil {
		return nil, err
	}
	n.inboxes[agent] = ib
	return ib, nil
}

func (n *network) hosts() map[string]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make(map[string]string, len(n.cfg.Hosts))
	for k, v := range n.cfg.Hosts {
		out[k] = v
	}
	return out
}

func (n *network) auditLine(actor, action, target, detail string) {
	n.auditMu.Lock()
	defer n.auditMu.Unlock()
	fmt.Fprintf(n.audit, "%s\t%s\t%s\t%s\t%s\n",
		time.Now().UTC().Format(time.RFC3339), actor, action, target, detail)
}

// ident is the resolved authentication result for a request.
type ident struct {
	Control bool   // holds the network control token
	NetTok  bool   // holds a network token (msg or control)
	Agent   string // agent name when an agent token was presented
}

// resolve maps a bearer token to an identity within a network.
func (n *network) resolve(tok string) (ident, bool) {
	if tok == "" {
		return ident{}, false
	}
	h := proto.HashToken(tok)
	n.mu.Lock()
	msgH := proto.HashToken(n.cfg.MsgToken)
	var ctlH string
	if n.cfg.ControlToken != "" {
		ctlH = proto.HashToken(n.cfg.ControlToken)
	}
	n.mu.Unlock()
	if ctlH != "" && subtle.ConstantTimeCompare([]byte(h), []byte(ctlH)) == 1 {
		return ident{Control: true, NetTok: true}, true
	}
	if subtle.ConstantTimeCompare([]byte(h), []byte(msgH)) == 1 {
		return ident{NetTok: true}, true
	}
	if rec, ok := n.reg.ByToken(h); ok {
		return ident{Agent: rec.Name}, true
	}
	return ident{}, false
}

// from returns the stamped sender identity for a request.
func (h *Hub) from(id ident) string {
	if id.Agent != "" {
		return id.Agent + "@" + h.Cfg.HostName
	}
	return "human@" + h.Cfg.HostName
}

// alive probes whether a registered agent is still what it was bound to.
func alive(rec store.AgentRec) bool {
	if rec.Pane != "" {
		if !tmux.PaneExists(rec.Pane) {
			return false
		}
		if rec.PID > 0 && rec.StartEpoch != "" {
			ep, err := tmux.ProcStartEpoch(rec.PID)
			return err == nil && ep == rec.StartEpoch
		}
		return true
	}
	if rec.PID > 0 && rec.StartEpoch != "" {
		ep, err := tmux.ProcStartEpoch(rec.PID)
		return err == nil && ep == rec.StartEpoch
	}
	return true // unbindable (no pane, no pid): trusted until deregistered
}

// deliverLocal appends an envelope to a local agent's inbox and fires the
// nudge engine. Fresh reports whether it was not a duplicate.
func (h *Hub) deliverLocal(n *network, agent string, env proto.Envelope) error {
	rec, ok := n.reg.Get(agent)
	if !ok {
		return fmt.Errorf("no such agent")
	}
	ib, err := n.inbox(agent)
	if err != nil {
		return err
	}
	_, fresh, err := ib.Append(env)
	if err != nil {
		return err
	}
	if fresh {
		h.maybeNudge(n, rec, ib)
	}
	return nil
}

// maybeNudge injects a one-line hint into an idle agent's pane when new
// mail lands. Never bodies — just a pointer. Suppressed while the agent
// long-polls, coalesced to one per nudgeEvery.
func (h *Hub) maybeNudge(n *network, rec store.AgentRec, ib *store.Inbox) {
	if rec.Pane == "" || ib.Pollers() > 0 {
		return
	}
	lag := ib.Lag()
	if lag <= 0 {
		return
	}
	n.mu.Lock()
	if time.Since(n.lastNudge[rec.Name]) < nudgeEvery {
		n.mu.Unlock()
		return
	}
	n.lastNudge[rec.Name] = time.Now()
	n.mu.Unlock()
	pane := rec.Pane
	line := fmt.Sprintf("hive: %d new message(s) — run: hive recv", lag)
	go func() {
		if !tmux.PaneExists(pane) {
			return
		}
		if tmux.SendKeysLiteral(pane, line) == nil {
			tmux.Enter(pane)
		}
	}()
}

// broadcastLocal delivers to every local agent except the sender.
func (h *Hub) broadcastLocal(n *network, env proto.Envelope, exceptFrom string) map[string]string {
	res := map[string]string{}
	for _, rec := range n.reg.List() {
		if rec.Name+"@"+h.Cfg.HostName == exceptFrom {
			continue
		}
		if err := h.deliverLocal(n, rec.Name, env); err != nil {
			res[rec.Name] = err.Error()
		} else {
			res[rec.Name] = "delivered"
		}
	}
	return res
}
