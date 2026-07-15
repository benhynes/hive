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
	// Generated identities disappear from discovery at lease expiry, but their
	// token/mailbox get a bounded recovery window for partitions and suspend
	// before irreversible retirement.
	ephemeralRetention = 24 * time.Hour
	// nudgeNotice is deliberately fixed and starts with a shell comment. A
	// peer can trigger a wake-up by sending mail, but cannot choose any bytes
	// typed into the pane; if the agent has exited back to a shell, Enter runs
	// an inert comment rather than a command.
	nudgeNotice = "# hive: unread messages waiting - call the hive_recv tool"
)

// Hub is one host's daemon state.
type Hub struct {
	Cfg    config.Config
	mu     sync.Mutex
	nets   map[string]*network
	client *http.Client // hub->hub calls
	ssh    *sshManager  // on-demand SSH hosts (transient remote daemons over tunnels)

	// killSessionFn is the control-backend seam used by hKill. Production
	// falls back to control.KillSession; tests replace it to hold teardown at a
	// deterministic point while a dead name is reclaimed.
	killSessionFn func(session, pane string) error
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
	h := &Hub{
		Cfg:    cfg,
		nets:   map[string]*network{},
		client: &http.Client{Timeout: 3500 * time.Millisecond},
	}
	h.ssh = newSSHManager(h)
	return h
}

// Shutdown releases hub-owned external resources: SSH-host tunnels and their
// transient remote daemons. Call it when the daemon stops.
func (h *Hub) Shutdown() { h.ssh.shutdown() }

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

// retireInbox removes a disposable identity's mailbox from memory and disk.
// The caller holds regMu, which excludes every production path that can open,
// append, or ack an inbox while the name is being made reusable.
func (n *network) retireInbox(agent string) error {
	n.mu.Lock()
	ib := n.inboxes[agent]
	delete(n.inboxes, agent)
	delete(n.lastNudge, agent)
	delete(n.lastNudgedLatest, agent)
	n.mu.Unlock()
	if ib != nil {
		return ib.Retire()
	}
	return store.RemoveInbox(n.dir, agent)
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
	Control   bool   // holds this hub's accepted control token
	NetTok    bool   // holds a network token (msg or control)
	Agent     string // agent name when an agent token was presented
	TokenHash string // exact agent-token generation resolved for this request
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
		return ident{Agent: rec.Name, TokenHash: h}, true
	}
	return ident{}, false
}

// ownsRegistration reports whether an agent-authenticated request still owns
// the exact registration generation it resolved to. Agent names are reusable:
// a token resolving before a long poll or slow request must not authorize work
// against a later claimant of the same name.
func (n *network) ownsRegistration(id ident, name string) bool {
	if id.Agent == "" || id.Agent != name || id.TokenHash == "" {
		return false
	}
	rec, ok := n.reg.Get(name)
	return ok && rec.TokenHash == id.TokenHash
}

func (h *Hub) killSession(session, pane string) error {
	if h.killSessionFn != nil {
		return h.killSessionFn(session, pane)
	}
	return control.KillSession(session, pane)
}

// from returns the stamped sender identity for a request.
func (h *Hub) from(id ident) string {
	if id.Agent != "" {
		return id.Agent + "@" + h.Cfg.HostName
	}
	return "human@" + h.Cfg.HostName
}

// leaseExpiredAt reports only renewable-presence expiry. Legacy records have
// no lease fields and therefore never expire through this path.
func leaseExpiredAt(rec store.AgentRec, now time.Time) bool {
	if rec.LeaseSeconds <= 0 && rec.LeaseExpires <= 0 {
		return false
	}
	return rec.LeaseExpires <= 0 || now.UnixMilli() >= rec.LeaseExpires
}

// discoverableAt keeps an expired generated identity out of rosters while its
// token and mailbox remain recoverable during the bounded retirement grace.
// A successful heartbeat extends its lease and makes it discoverable again.
func discoverableAt(rec store.AgentRec, now time.Time) bool {
	return !rec.Ephemeral || !leaseExpiredAt(rec, now)
}

// alive probes whether a registered agent is still what it was bound to.
func alive(rec store.AgentRec) bool { return aliveAt(rec, time.Now()) }

func aliveAt(rec store.AgentRec, now time.Time) bool {
	// A lease is an additional liveness condition, not a replacement for a
	// pane/PID binding. Legacy records have a zero expiry and retain their old
	// trusted-until-deregistered behavior when otherwise unbound.
	if leaseExpiredAt(rec, now) {
		return false
	}
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
	n.regMu.Lock()
	rec, ok := n.reg.Get(agent)
	if !ok {
		n.regMu.Unlock()
		return fmt.Errorf("no such agent")
	}
	ib, err := n.inbox(agent)
	if err != nil {
		n.regMu.Unlock()
		return err
	}
	_, fresh, err := ib.Append(env)
	n.regMu.Unlock()
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

// nudge injects a fixed, shell-inert hint into an explicitly opted-in agent's
// idle pane. It is the single choke point for both the delivery path and the
// sweeper, so per-agent rate limiting is consistent across them.
//
// Gating, in order:
//   - no explicit nudge opt-in, no pane, or the agent is actively long-polling
//     its own inbox → never inject terminal input.
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
	if !rec.Nudge || rec.Pane == "" || ib.Pollers() > 0 {
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

	line := nudgeLine()
	pane := rec.Pane
	go func() {
		// Serialize the final ownership check and terminal writes with register,
		// deregister, spawn, and kill. Otherwise an already-queued Nudge=true
		// job could type into a replacement registration that opted out while
		// reusing the same pane.
		n.regMu.Lock()
		defer n.regMu.Unlock()
		current, ok := n.reg.Get(rec.Name)
		if !ok || current.TokenHash != rec.TokenHash || current.Pane != pane || !current.Nudge {
			return
		}
		// Delivery may have queued this goroutine while the process was exiting
		// or while the user/model was composing a draft. A conservatively
		// recognized empty prompt is required as defense in depth; it cannot
		// eliminate the capture-to-Enter race, which is why nudging is opt-in.
		if !alive(current) {
			return
		}
		screen, err := control.Capture(pane, 0)
		if err != nil || !emptyPanePrompt(screen) {
			return
		}
		if !alive(current) {
			return
		}
		if control.SendKeysLiteral(pane, line) == nil {
			control.Enter(pane)
		}
	}()
}

// nudgeLine is a tiny seam for testing the only text automatic delivery is
// allowed to type into a pane. It intentionally accepts no envelope data.
func nudgeLine() string { return nudgeNotice }

// emptyPanePrompt recognizes only a small set of conventional empty prompts.
// The final nonblank rendered line must be exactly the prompt glyph: a suffix,
// draft, status line, or unknown TUI is rejected. False negatives merely defer
// a nudge; false positives can submit user/model input, so stay conservative.
func emptyPanePrompt(screen string) bool {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		switch line {
		case "$", "#", "%", "❯", "›":
			return true
		default:
			return false
		}
	}
	return false
}

// pruneExpiredEphemeral serializes removal with registration, heartbeat, and
// deregistration ownership changes. The store method removes only disposable
// generated identities; named expired records remain available for resume.
func pruneExpiredEphemeral(n *network, now time.Time) ([]string, error) {
	n.regMu.Lock()
	defer n.regMu.Unlock()
	return n.reg.PruneExpiredEphemeral(now, ephemeralRetention, n.retireInbox)
}

// SweepNudges periodically re-checks every locally-registered, opted-in,
// pane-bound agent and re-nudges any that still hold unread mail. It also removes expired
// generated identities so unclean exits do not permanently clutter discovery.
// This is the re-arm
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
			if _, err := pruneExpiredEphemeral(n, time.Now()); err != nil && n.audit != nil {
				n.auditLine("daemon (sweep)", "prune", "ephemeral registrations", "error: "+err.Error())
			}
			for _, rec := range n.reg.List() {
				if !rec.Nudge || rec.Pane == "" {
					continue
				}
				n.regMu.Lock()
				current, ok := n.reg.Get(rec.Name)
				if !ok || current.TokenHash != rec.TokenHash {
					n.regMu.Unlock()
					continue
				}
				ib, err := n.inbox(rec.Name)
				n.regMu.Unlock()
				if err != nil {
					continue
				}
				// Check for unread mail (in-memory) before liveness: alive()
				// shells out to tmux + ps, and most agents have nothing pending,
				// so gating the subprocess spawns on Lag keeps the sweep from
				// costing 2 execs per agent per tick across the whole roster.
				if ib.Lag() <= 0 || !alive(rec) {
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
	now := time.Now()
	for _, rec := range n.reg.List() {
		if !discoverableAt(rec, now) {
			continue
		}
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
