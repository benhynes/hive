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
//   - CONTROL is possession of the hub's configured control token. It may
//     be shared network-wide or bound to one host. Spawning with
//     --grant-control injects it as HIVE_CONTROL_TOKEN.
package hub

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/control"
	"github.com/benhynes/hive/internal/proto"
	"github.com/benhynes/hive/internal/store"
)

const (
	// nudgeReminderEvery is how often an agent that is still sitting on
	// unread mail (it ignored the last nudge) is reminded.
	nudgeReminderEvery = 30 * time.Second
	// nudgeMinGap coalesces a burst of near-simultaneous arrivals into one
	// nudge, so ten messages landing together don't type ten lines.
	nudgeMinGap = 2 * time.Second
	// nudgeSweepEvery is how often the sweeper re-checks idle agents. It
	// catches mail that arrived inside nudgeMinGap and was never followed by
	// another delivery — the case that used to stall unbounded.
	nudgeSweepEvery = 3 * time.Second
	// nudgePreviewMax caps how many bytes of a message body are typed into a
	// pane. The full body still comes via recv; this is just enough to act on.
	nudgePreviewMax = 240
)

// Hub is one host's daemon state.
type Hub struct {
	Cfg    config.Config
	mu     sync.Mutex
	nets   map[string]*network
	client *http.Client // hub->hub calls
}

type network struct {
	name    string
	dir     string
	reg     *store.Registry
	persist *store.PersistStore // declared sessions the daemon keeps alive

	// regMu serializes name claims (register/spawn): the aliveness
	// probes and tmux calls between the taken-check and the registry
	// Put would otherwise race, letting two claimants both succeed and
	// the loser silently revoke the winner's token.
	regMu sync.Mutex

	mu        sync.Mutex // guards cfg, inboxes, lastNudge, lastNudgedLatest
	cfg       config.NetConfig
	inboxes   map[string]*store.Inbox
	lastNudge map[string]time.Time
	// lastNudgedLatest is the inbox Latest() seq an agent was last nudged
	// about. New mail past it re-arms the nudge even inside the anti-spam
	// floor; it is what makes an in-window message get announced instead of
	// silently stalling until the next unrelated delivery.
	lastNudgedLatest map[string]int64

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
	persist, err := store.OpenPersist(dir)
	if err != nil {
		return nil, err
	}
	audit, err := os.OpenFile(filepath.Join(dir, "audit.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	n := &network{
		name: name, dir: dir, reg: reg, persist: persist, cfg: cfg,
		inboxes:          map[string]*store.Inbox{},
		lastNudge:        map[string]time.Time{},
		lastNudgedLatest: map[string]int64{},
		audit:            audit,
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
	Control bool   // holds this hub's accepted control token
	NetTok  bool   // holds a network token (msg or control)
	Agent   string // agent name when an agent token was presented
}

// resolve maps a bearer token to an identity within a network.
func (n *network) resolve(tok, host string) (ident, bool) {
	if tok == "" {
		return ident{}, false
	}
	h := proto.HashToken(tok)
	n.mu.Lock()
	msgH := proto.HashToken(n.cfg.MsgToken)
	var ctlH string
	if ctl := n.cfg.ControlFor(host); ctl != "" {
		ctlH = proto.HashToken(ctl)
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
	if rec.Pane != "" && !control.PaneExists(rec.Pane) {
		return false
	}
	if rec.PID > 0 && rec.StartEpoch != "" {
		ep, err := control.ProcStartEpoch(rec.PID)
		return err == nil && ep == rec.StartEpoch
	}
	return true // unbindable (no pid epoch): trusted until deregistered
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

// maybeNudge is the delivery-path nudge: a message just landed, so try to
// tell the agent. The sweeper (SweepNudges) is the safety net for mail that
// slips through the anti-spam gate here and is never followed by another
// delivery.
func (h *Hub) maybeNudge(n *network, rec store.AgentRec, ib *store.Inbox) {
	h.nudge(n, rec, ib)
}

// nudge injects a hint (and a preview of the waiting mail) into an idle
// agent's pane. It is the single choke point for both the delivery path and
// the sweeper, so per-agent rate limiting is consistent across them.
//
// Gating, in order:
//   - no pane, or the agent is actively long-polling its own inbox → the
//     agent will see the mail itself; injecting would just race its TUI.
//   - nothing unread → nothing to say.
//   - within nudgeMinGap of the last nudge → coalesce a burst.
//   - already nudged about this exact latest seq, and the reminder interval
//     has not elapsed → don't repeat ourselves.
//
// The last rule is what fixes the old unbounded stall: a message that bumps
// Latest past what we last announced always passes, so an in-window arrival
// is announced on the next sweep instead of being lost until some unrelated
// later delivery happened to re-trigger the code path.
func (h *Hub) nudge(n *network, rec store.AgentRec, ib *store.Inbox) {
	if rec.Pane == "" || ib.Pollers() > 0 {
		return
	}
	lag := ib.Lag()
	if lag <= 0 {
		return
	}
	latest := ib.Latest()

	n.mu.Lock()
	since := time.Since(n.lastNudge[rec.Name])
	seen := n.lastNudgedLatest[rec.Name]
	if since < nudgeMinGap {
		n.mu.Unlock()
		return
	}
	if latest <= seen && since < nudgeReminderEvery {
		n.mu.Unlock()
		return
	}
	n.lastNudge[rec.Name] = time.Now()
	n.lastNudgedLatest[rec.Name] = latest
	n.mu.Unlock()

	line := nudgeLine(ib, lag)
	pane := rec.Pane
	go func() {
		if !control.PaneExists(pane) {
			return
		}
		if control.SendKeysLiteral(pane, line) == nil {
			control.Enter(pane)
		}
	}()
}

// nudgeLine builds the text typed into the pane: the oldest unread message's
// sender and a preview of its body, so the agent can start acting without a
// round trip through recv. Extra unread mail is summarized as a count.
func nudgeLine(ib *store.Inbox, lag int64) string {
	res := ib.Read(ib.Cursor(), 1)
	if len(res.Msgs) == 0 {
		// Raced with a compaction/ack; fall back to the pointer form.
		return fmt.Sprintf("hive: %d new message(s) — call the hive_recv tool", lag)
	}
	e := res.Msgs[0].Env
	kind := ""
	if e.Kind == proto.KindAsk {
		kind = "asks" // asks are blocking; flag them
	} else {
		kind = "says"
	}
	line := fmt.Sprintf("hive: %s %s: %s", e.From, kind, preview(e.Body))
	if lag > 1 {
		line += fmt.Sprintf("  (+%d more — hive_recv)", lag-1)
	}
	return line
}

// preview collapses a body to a single, printable, length-capped line safe to
// type into a pane. The full body is always still available via recv; this is
// only enough to act on.
func preview(body string) string {
	// One line: a raw newline typed into a TUI would submit early or inject a
	// stray Enter, so fold all whitespace runs to single spaces.
	s := strings.Join(strings.Fields(body), " ")
	if len(s) > nudgePreviewMax {
		// Trim on a rune boundary, not mid-codepoint.
		cut := nudgePreviewMax
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// SweepNudges periodically re-checks every locally-registered, pane-bound
// agent and re-nudges any that still hold unread mail. This is the re-arm
// that turns the old unbounded stall (a message that arrived during the
// anti-spam window and was never followed by another delivery) into one
// bounded by nudgeReminderEvery. It shares the nudge choke point, so the
// per-agent rate limit still holds.
func (h *Hub) SweepNudges(ctx context.Context) {
	tick := time.NewTicker(nudgeSweepEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		h.mu.Lock()
		nets := make([]*network, 0, len(h.nets))
		for _, n := range h.nets {
			nets = append(nets, n)
		}
		h.mu.Unlock()
		for _, n := range nets {
			for _, rec := range n.reg.List() {
				if rec.Pane == "" || !alive(rec) {
					continue
				}
				ib, err := n.inbox(rec.Name)
				if err != nil {
					continue
				}
				h.nudge(n, rec, ib)
			}
		}
	}
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
