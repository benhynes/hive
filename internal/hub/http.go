package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/control"
	"github.com/benhynes/hive/internal/proto"
	"github.com/benhynes/hive/internal/store"
)

const (
	// maxWait caps long-poll hold time per request.
	maxWait = 25 * time.Second
	// maxLeaseSeconds prevents an accidental effectively-permanent lease.
	// Zero remains meaningful: it requests the legacy, unleased behavior.
	maxLeaseSeconds = 60 * 60
)

type errResp struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, format string, a ...any) {
	writeJSON(w, code, errResp{Error: fmt.Sprintf(format, a...)})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, 64*1024)
	if err := json.NewDecoder(body).Decode(v); err != nil {
		httpErr(w, 400, "bad json: %v", err)
		return false
	}
	return true
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(tok)
}

// access levels per endpoint
const (
	accAny     = iota // any valid token in the network
	accNetTok         // network token required (register, deliver)
	accControl        // network control token required
)

type netHandler func(w http.ResponseWriter, r *http.Request, n *network, id ident)

func (h *Hub) withNet(level int, fn netHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, err := h.net(r.PathValue("net"))
		if err != nil {
			httpErr(w, 404, "unknown network")
			return
		}
		id, ok := n.resolve(bearer(r), h.Cfg.HostName)
		if !ok {
			httpErr(w, 401, "bad or missing token")
			return
		}
		switch level {
		case accNetTok:
			if !id.NetTok {
				httpErr(w, 403, "network token required")
				return
			}
		case accControl:
			if !id.Control {
				httpErr(w, 403, "control layer required")
				return
			}
		}
		fn(w, r, n, id)
	}
}

// Handler returns the full /v1 API mux.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"api": "hive", "v": 1, "host": h.Cfg.HostName,
			"features": []string{
				"leases", "ephemeral_registration", "release_presence",
				"explicit_nudge", "versioned_pane_mutations",
			},
		})
	})
	mux.HandleFunc("GET /metrics", h.hMetrics)
	mux.HandleFunc("POST /v1/nets/{net}/register", h.withNet(accNetTok, h.hRegister))
	// Versioned pane mutation paths are an atomic compatibility boundary: an
	// older implicitly-nudging daemon returns 404 instead of creating a pane.
	mux.HandleFunc("POST /v1/nets/{net}/register/v2", h.withNet(accNetTok, h.hRegister))
	mux.HandleFunc("POST /v1/nets/{net}/heartbeat", h.withNet(accAny, h.hHeartbeat))
	mux.HandleFunc("POST /v1/nets/{net}/release", h.withNet(accAny, h.hRelease))
	mux.HandleFunc("POST /v1/nets/{net}/deregister", h.withNet(accAny, h.hDeregister))
	mux.HandleFunc("GET /v1/nets/{net}/agents", h.withNet(accAny, h.hAgents))
	mux.HandleFunc("POST /v1/nets/{net}/send", h.withNet(accAny, h.hSend))
	mux.HandleFunc("POST /v1/nets/{net}/deliver", h.withNet(accNetTok, h.hDeliver))
	mux.HandleFunc("GET /v1/nets/{net}/inbox", h.withNet(accAny, h.hInbox))
	mux.HandleFunc("POST /v1/nets/{net}/ack", h.withNet(accAny, h.hAck))
	mux.HandleFunc("GET /v1/nets/{net}/hosts", h.withNet(accAny, h.hHostsGet))
	mux.HandleFunc("POST /v1/nets/{net}/hosts", h.withNet(accControl, h.hHostsPost))
	mux.HandleFunc("POST /v1/nets/{net}/control/rotate", h.withNet(accControl, h.hRotateControl))
	mux.HandleFunc("POST /v1/nets/{net}/spawn", h.withNet(accControl, h.hSpawn))
	mux.HandleFunc("POST /v1/nets/{net}/spawn/v2", h.withNet(accControl, h.hSpawn))
	mux.HandleFunc("POST /v1/nets/{net}/keys", h.withNet(accControl, h.hKeys))
	mux.HandleFunc("GET /v1/nets/{net}/read", h.withNet(accControl, h.hRead))
	mux.HandleFunc("POST /v1/nets/{net}/kill", h.withNet(accControl, h.hKill))
	return mux
}

// ListenAndServe runs the daemon until the context is cancelled.
func (h *Hub) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              net.JoinHostPort(h.Cfg.Bind, strconv.Itoa(h.Cfg.Port)),
		Handler:           h.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// actor names the caller for audit lines. Agent tokens carry identity;
// network-token callers may hint via X-Hive-Actor (advisory — control
// token holders are fully trusted anyway).
func (h *Hub) actor(r *http.Request, id ident) string {
	if id.Agent != "" {
		return id.Agent + "@" + h.Cfg.HostName
	}
	if a := r.Header.Get("X-Hive-Actor"); a != "" && len(a) <= 80 {
		return a + " (net-token)"
	}
	return "human (net-token)"
}

// ---- registration ----

type registerReq struct {
	Name         string `json:"name"`
	Pane         string `json:"pane,omitempty"` // caller's $TMUX_PANE, verified here
	Nudge        bool   `json:"nudge,omitempty"`
	PID          int    `json:"pid,omitempty"` // fallback liveness binding
	LeaseSeconds int    `json:"lease_seconds,omitempty"`
	Ephemeral    bool   `json:"ephemeral,omitempty"`
}

type registerResp struct {
	Agent        string `json:"agent"`
	Token        string `json:"token"`
	Nudge        bool   `json:"nudge,omitempty"`
	NudgePolicy  string `json:"nudge_policy"`
	LeaseSeconds int    `json:"lease_seconds,omitempty"`
	LeaseExpires int64  `json:"lease_expires,omitempty"`
	Ephemeral    bool   `json:"ephemeral,omitempty"`
}

func (h *Hub) hRegister(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	var req registerReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Nudge && req.Pane == "" {
		httpErr(w, 400, "nudge requires an explicitly bound pane")
		return
	}
	// Binding a pane grants the hub the ability to inject keystrokes into it.
	// A shared MSG credential may bootstrap message-only/PID-bound agents, but
	// it must not be enough to point that control path at an arbitrary pane.
	if req.Pane != "" && !id.Control {
		httpErr(w, 403, "control layer required to bind a pane")
		return
	}
	if !proto.ValidName(req.Name) {
		httpErr(w, 400, "bad agent name (want [a-z0-9][a-z0-9_-]*, ≤32)")
		return
	}
	if req.LeaseSeconds < 0 || req.LeaseSeconds > maxLeaseSeconds {
		httpErr(w, 400, "lease_seconds must be between 0 and %d", maxLeaseSeconds)
		return
	}
	if req.Ephemeral && req.LeaseSeconds == 0 {
		httpErr(w, 400, "ephemeral registration requires a positive lease_seconds")
		return
	}
	n.regMu.Lock()
	defer n.regMu.Unlock()
	now := time.Now()
	rec := store.AgentRec{Name: req.Name, Nudge: req.Nudge, Ephemeral: req.Ephemeral, Registered: now.UnixMilli()}
	if req.LeaseSeconds > 0 {
		rec.LeaseSeconds = req.LeaseSeconds
		rec.LastSeen = now.UnixMilli()
		rec.LeaseExpires = now.Add(time.Duration(req.LeaseSeconds) * time.Second).UnixMilli()
	}
	if err := control.AllowClientPane(req.Pane); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	if req.Pane != "" {
		// PanePID verifies liveness internally, so no separate existence
		// probe is needed.
		pid, err := control.PanePID(req.Pane)
		if err != nil {
			httpErr(w, 400, "pane %s: %v", req.Pane, err)
			return
		}
		epoch, err := control.ProcStartEpoch(pid)
		if err != nil {
			httpErr(w, 400, "pane process gone")
			return
		}
		rec.Pane, rec.PID, rec.StartEpoch = req.Pane, pid, epoch
	} else if req.PID > 0 {
		epoch, err := control.ProcStartEpoch(req.PID)
		if err != nil {
			httpErr(w, 400, "pid %d not found", req.PID)
			return
		}
		rec.PID, rec.StartEpoch = req.PID, epoch
	}
	if old, ok := n.reg.Get(req.Name); ok {
		if alive(old) {
			httpErr(w, 409, "name %q is taken by a live agent", req.Name)
			return
		}
		if old.Ephemeral {
			if err := n.retireInbox(req.Name); err != nil {
				httpErr(w, 500, "retire expired mailbox: %v", err)
				return
			}
		}
	}
	tok := proto.NewToken()
	rec.TokenHash = proto.HashToken(tok)
	if err := n.reg.Put(rec); err != nil {
		httpErr(w, 500, "registry: %v", err)
		return
	}
	n.auditLine(h.actor(r, id), "register", req.Name+"@"+h.Cfg.HostName, "")
	writeJSON(w, 200, registerResp{
		Agent: req.Name + "@" + h.Cfg.HostName, Token: tok,
		Nudge: req.Nudge, NudgePolicy: "explicit",
		LeaseSeconds: rec.LeaseSeconds, LeaseExpires: rec.LeaseExpires,
		Ephemeral: rec.Ephemeral,
	})
}

type heartbeatResp struct {
	Agent        string `json:"agent"`
	LeaseSeconds int    `json:"lease_seconds,omitempty"`
	LeaseExpires int64  `json:"lease_expires,omitempty"`
}

func (h *Hub) hHeartbeat(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	// A network credential can create an identity, but only the minted agent
	// credential can assert that identity is still present.
	if id.Agent == "" {
		httpErr(w, 403, "only agents can heartbeat their own registration")
		return
	}
	// Serialize renewal with name claims. Otherwise a heartbeat could land
	// between an expired-name check and its replacement, or an already-resolved
	// old token could accidentally extend the replacement's lease.
	n.regMu.Lock()
	rec, ok, err := n.reg.RenewLease(id.Agent, id.TokenHash, time.Now())
	n.regMu.Unlock()
	if err != nil {
		httpErr(w, 500, "registry: %v", err)
		return
	}
	if !ok {
		httpErr(w, 404, "no such agent")
		return
	}
	writeJSON(w, 200, heartbeatResp{
		Agent:        rec.Name + "@" + h.Cfg.HostName,
		LeaseSeconds: rec.LeaseSeconds, LeaseExpires: rec.LeaseExpires,
	})
}

// hRelease is the clean-shutdown counterpart to heartbeat for a stable
// managed identity. It makes presence false immediately but deliberately
// retains the address and mailbox so peers can queue work while it is offline.
func (h *Hub) hRelease(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	if id.Agent == "" {
		httpErr(w, 403, "only agents can release their own presence lease")
		return
	}
	n.regMu.Lock()
	rec, ok, err := n.reg.ReleaseLease(id.Agent, id.TokenHash, time.Now())
	n.regMu.Unlock()
	if err != nil {
		if errors.Is(err, store.ErrNotRetainedLease) {
			httpErr(w, 400, "release: %v", err)
		} else {
			httpErr(w, 500, "registry: %v", err)
		}
		return
	}
	if !ok {
		httpErr(w, 409, "agent registration was replaced")
		return
	}
	writeJSON(w, 200, heartbeatResp{
		Agent: rec.Name + "@" + h.Cfg.HostName, LeaseSeconds: rec.LeaseSeconds,
		LeaseExpires: rec.LeaseExpires,
	})
}

func (h *Hub) hDeregister(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSuffix(req.Name, "@"+h.Cfg.HostName)
	if name == "" && id.Agent != "" {
		name = id.Agent
	}
	if !id.Control && id.Agent != name {
		httpErr(w, 403, "can only deregister yourself without the control token")
		return
	}
	// Serialize deletion with name claims and re-check ownership under that
	// lock. An expired client's request may have authenticated just before a
	// replacement claimed the same name; it must not delete the replacement.
	n.regMu.Lock()
	defer n.regMu.Unlock()
	rec, ok := n.reg.Get(name)
	if !ok {
		httpErr(w, 404, "no such agent")
		return
	}
	if !id.Control && rec.TokenHash != id.TokenHash {
		httpErr(w, 409, "agent registration was replaced")
		return
	}
	if rec.Ephemeral {
		if err := n.retireInbox(name); err != nil {
			httpErr(w, 500, "retire ephemeral mailbox: %v", err)
			return
		}
	}
	if err := n.reg.Delete(name); err != nil {
		httpErr(w, 500, "registry: %v", err)
		return
	}
	n.auditLine(h.actor(r, id), "deregister", name+"@"+h.Cfg.HostName, "")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- discovery ----

type agentInfo struct {
	Agent        string `json:"agent"`
	Alive        bool   `json:"alive"`
	Controllable bool   `json:"controllable"`
	Nudgeable    bool   `json:"nudgeable"`
	Ephemeral    bool   `json:"ephemeral,omitempty"`
	Spawned      bool   `json:"spawned,omitempty"`
	Registered   int64  `json:"registered"`
	LastSeen     int64  `json:"last_seen,omitempty"`
	LeaseExpires int64  `json:"lease_expires,omitempty"`
}

type agentsResp struct {
	Self        string            `json:"self,omitempty"`
	Agents      []agentInfo       `json:"agents"`
	Unreachable map[string]string `json:"unreachable,omitempty"`
}

func (h *Hub) localAgents(n *network) []agentInfo {
	var out []agentInfo
	now := time.Now()
	for _, rec := range n.reg.List() {
		if !discoverableAt(rec, now) {
			continue
		}
		out = append(out, agentInfo{
			Agent:        rec.Name + "@" + h.Cfg.HostName,
			Alive:        aliveAt(rec, now),
			Controllable: rec.Pane != "",
			Nudgeable:    rec.Pane != "" && rec.Nudge,
			Ephemeral:    rec.Ephemeral,
			Spawned:      rec.Spawned,
			Registered:   rec.Registered,
			LastSeen:     rec.LastSeen,
			LeaseExpires: rec.LeaseExpires,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

func (h *Hub) hAgents(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	resp := agentsResp{Agents: h.localAgents(n), Unreachable: map[string]string{}}
	if id.Agent != "" {
		resp.Self = id.Agent + "@" + h.Cfg.HostName
	}
	if r.URL.Query().Get("local") == "1" {
		writeJSON(w, 200, resp)
		return
	}
	n.mu.Lock()
	msgTok := n.cfg.MsgToken
	n.mu.Unlock()
	var wg sync.WaitGroup
	var mu sync.Mutex
	for host, addr := range n.hosts() {
		if host == h.Cfg.HostName {
			continue
		}
		wg.Add(1)
		go func(host, addr string) {
			defer wg.Done()
			var rr agentsResp
			err := h.rpc("GET", addr, "/v1/nets/"+n.name+"/agents?local=1", msgTok, nil, &rr)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				resp.Unreachable[host] = err.Error()
				return
			}
			resp.Agents = append(resp.Agents, rr.Agents...)
		}(host, addr)
	}
	wg.Wait()
	sort.Slice(resp.Agents, func(i, j int) bool { return resp.Agents[i].Agent < resp.Agents[j].Agent })
	writeJSON(w, 200, resp)
}

// ---- messaging ----

type sendReq struct {
	To     string `json:"to"`
	Kind   string `json:"kind,omitempty"`
	Body   string `json:"body"`
	CorrID string `json:"corr_id,omitempty"`
}

type sendResp struct {
	ID      string            `json:"id"`
	Results map[string]string `json:"results"`
}

func (h *Hub) hSend(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	var req sendReq
	if !readJSON(w, r, &req) {
		return
	}
	// Request bodies can be streamed arbitrarily slowly. If this agent's name
	// was reclaimed after withNet resolved its token but before the body
	// completed, it must not send under the replacement's display identity.
	// Registry.Get is the operation's ownership linearization point: a reclaim
	// before it is rejected; a reclaim after it follows an already-authorized
	// send.
	if id.Agent != "" && !n.ownsRegistration(id, id.Agent) {
		httpErr(w, 409, "agent registration was replaced")
		return
	}
	if req.Kind == "" {
		req.Kind = proto.KindMsg
	}
	from := h.from(id)
	env := proto.Envelope{
		From: from, To: req.To, Kind: req.Kind,
		Body: req.Body, CorrID: req.CorrID, TS: time.Now().UnixMilli(),
	}
	env.ID = proto.NewID(env.From, env.To, env.Kind, env.Body, env.CorrID, env.TS)
	if err := env.Validate(); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	resp := sendResp{ID: env.ID, Results: map[string]string{}}

	if env.To == proto.Broadcast {
		// Exclude the sender only when it IS a local agent: net-token
		// senders stamp as "human@host", which must not skip a real
		// agent that happens to be named "human".
		except := ""
		if id.Agent != "" {
			except = from
		}
		for a, s := range h.broadcastLocal(n, env, except) {
			resp.Results[a+"@"+h.Cfg.HostName] = s
		}
		n.mu.Lock()
		msgTok := n.cfg.MsgToken
		n.mu.Unlock()
		var wg sync.WaitGroup
		var mu sync.Mutex
		for host, addr := range n.hosts() {
			if host == h.Cfg.HostName {
				continue
			}
			wg.Add(1)
			go func(host, addr string) {
				defer wg.Done()
				results, err := h.forwardDeliver(addr, n.name, msgTok, env)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					resp.Results["@"+host] = "unreachable: " + err.Error()
					return
				}
				for a, s := range results {
					resp.Results[a+"@"+host] = s
				}
			}(host, addr)
		}
		wg.Wait()
		writeJSON(w, 200, resp)
		return
	}

	name, host, _ := proto.SplitAgent(env.To) // Validate() already checked shape
	if host == h.Cfg.HostName {
		if err := h.deliverLocal(n, name, env); err != nil {
			resp.Results[env.To] = "undeliverable: " + err.Error()
		} else {
			resp.Results[env.To] = "delivered"
		}
		writeJSON(w, 200, resp)
		return
	}
	addr, ok := n.hosts()[host]
	if !ok {
		resp.Results[env.To] = "undeliverable: unknown host " + host
		writeJSON(w, 200, resp)
		return
	}
	n.mu.Lock()
	msgTok := n.cfg.MsgToken
	n.mu.Unlock()
	results, err := h.forwardDeliver(addr, n.name, msgTok, env)
	if err != nil {
		resp.Results[env.To] = "undeliverable: " + err.Error()
	} else if s, ok := results[name]; ok {
		resp.Results[env.To] = s
	} else {
		resp.Results[env.To] = "undeliverable: no result from remote hub"
	}
	writeJSON(w, 200, resp)
}

type deliverReq struct {
	Env proto.Envelope `json:"env"`
}

// hDeliver accepts a hub-to-hub forward. It only ever delivers locally —
// never re-forwards — which is what makes @all loop-free.
func (h *Hub) hDeliver(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	var req deliverReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := req.Env.Validate(); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	results := map[string]string{}
	if req.Env.To == proto.Broadcast {
		results = h.broadcastLocal(n, req.Env, req.Env.From)
	} else {
		name, host, _ := proto.SplitAgent(req.Env.To)
		if host != h.Cfg.HostName {
			httpErr(w, 400, "misrouted: %s is not on host %s", req.Env.To, h.Cfg.HostName)
			return
		}
		if err := h.deliverLocal(n, name, req.Env); err != nil {
			results[name] = err.Error()
		} else {
			results[name] = "delivered"
		}
	}
	writeJSON(w, 200, map[string]any{"results": results})
}

func (h *Hub) hInbox(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	q := r.URL.Query()
	agent := id.Agent
	if qa := q.Get("agent"); qa != "" {
		qa = strings.TrimSuffix(qa, "@"+h.Cfg.HostName)
		if qa != id.Agent && !id.Control {
			httpErr(w, 403, "control layer required to read another agent's inbox")
			return
		}
		agent = qa
	}
	if agent == "" {
		httpErr(w, 400, "no agent identity: register first, or pass ?agent= with the control token")
		return
	}
	n.regMu.Lock()
	rec, ok := n.reg.Get(agent)
	if !ok {
		n.regMu.Unlock()
		httpErr(w, 404, "no such agent")
		return
	}
	if id.Agent != "" && (id.Agent != agent || rec.TokenHash != id.TokenHash) {
		n.regMu.Unlock()
		httpErr(w, 409, "agent registration was replaced")
		return
	}
	ib, err := n.inbox(agent)
	n.regMu.Unlock()
	if err != nil {
		httpErr(w, 500, "inbox: %v", err)
		return
	}
	after := ib.Cursor()
	if s := q.Get("after"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			httpErr(w, 400, "bad after")
			return
		}
		after = v
	}
	max, _ := strconv.Atoi(q.Get("max"))
	if max <= 0 || max > 500 {
		max = 100
	}
	if q.Get("stat") == "1" {
		res := ib.Read(after, 1)
		res.Msgs = nil
		if id.Agent != "" && !n.ownsRegistration(id, agent) {
			httpErr(w, 409, "agent registration was replaced")
			return
		}
		writeJSON(w, 200, res)
		return
	}
	wait, _ := strconv.Atoi(q.Get("wait"))
	res := ib.Read(after, max)
	if len(res.Msgs) == 0 && wait > 0 {
		d := time.Duration(wait) * time.Second
		if d > maxWait {
			d = maxWait
		}
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		res = ib.Wait(ctx, after, max)
	}
	// The token was resolved before a possible long poll. The name may have
	// expired and been reclaimed while Wait was blocked, in which case res can
	// contain the replacement's mail. Never return it to the stale generation.
	if id.Agent != "" && !n.ownsRegistration(id, agent) {
		httpErr(w, 409, "agent registration was replaced")
		return
	}
	writeJSON(w, 200, res)
}

func (h *Hub) hAck(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	if id.Agent == "" {
		httpErr(w, 403, "only agents ack their own inbox")
		return
	}
	var req struct {
		Seq int64 `json:"seq"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	// Serialize the final ownership check and cursor mutation with name
	// reclaim. Otherwise an old token can authenticate, stall while its body is
	// read, then advance the replacement's cursor.
	n.regMu.Lock()
	if !n.ownsRegistration(id, id.Agent) {
		n.regMu.Unlock()
		httpErr(w, 409, "agent registration was replaced")
		return
	}
	ib, err := n.inbox(id.Agent)
	if err != nil {
		n.regMu.Unlock()
		httpErr(w, 500, "inbox: %v", err)
		return
	}
	if err := ib.Ack(req.Seq); err != nil {
		n.regMu.Unlock()
		httpErr(w, 400, "%v", err)
		return
	}
	cursor := ib.Cursor()
	n.regMu.Unlock()
	writeJSON(w, 200, map[string]any{"cursor": cursor})
}

// ---- hosts ----

func (h *Hub) hHostsGet(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	writeJSON(w, 200, map[string]any{"self": h.Cfg.HostName, "hosts": n.hosts()})
}

func (h *Hub) hHostsPost(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	var req struct {
		Op   string          `json:"op"`
		Name string          `json:"name"`
		Addr string          `json:"addr,omitempty"`
		SSH  *config.SSHHost `json:"ssh,omitempty"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !proto.ValidName(req.Name) {
		httpErr(w, 400, "bad host name")
		return
	}

	// SSH-host registration is metadata-only and may tear down a live tunnel,
	// so it runs outside the config lock (bring-up/teardown do their own).
	switch req.Op {
	case "add-ssh":
		if req.SSH == nil || req.SSH.Target == "" {
			httpErr(w, 400, "add-ssh needs an ssh target")
			return
		}
		n.mu.Lock()
		if n.cfg.SSHHosts == nil {
			n.cfg.SSHHosts = map[string]config.SSHHost{}
		}
		n.cfg.SSHHosts[req.Name] = *req.SSH
		err := config.SaveNet(n.cfg)
		n.mu.Unlock()
		if err != nil {
			httpErr(w, 500, "save: %v", err)
			return
		}
		n.auditLine(h.actor(r, id), "hosts-add-ssh", req.Name, req.SSH.Target)
		writeJSON(w, 200, map[string]any{"self": h.Cfg.HostName, "ssh_host": req.Name})
		return
	case "rm-ssh":
		h.ssh.teardown(n.name, req.Name) // close any live tunnel + remote daemon
		n.mu.Lock()
		delete(n.cfg.SSHHosts, req.Name)
		delete(n.cfg.Hosts, req.Name) // the tunnel's peer entry, if up
		err := config.SaveNet(n.cfg)
		n.mu.Unlock()
		if err != nil {
			httpErr(w, 500, "save: %v", err)
			return
		}
		n.auditLine(h.actor(r, id), "hosts-rm-ssh", req.Name, "")
		writeJSON(w, 200, map[string]any{"self": h.Cfg.HostName, "removed": req.Name})
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	switch req.Op {
	case "add":
		if _, _, err := net.SplitHostPort(req.Addr); err != nil {
			httpErr(w, 400, "bad addr (want host:port): %v", err)
			return
		}
		n.cfg.Hosts[req.Name] = req.Addr
	case "rm":
		delete(n.cfg.Hosts, req.Name)
	default:
		httpErr(w, 400, "op must be add, rm, add-ssh, or rm-ssh")
		return
	}
	if err := config.SaveNet(n.cfg); err != nil {
		httpErr(w, 500, "save: %v", err)
		return
	}
	n.auditLine(h.actor(r, id), "hosts-"+req.Op, req.Name, req.Addr)
	writeJSON(w, 200, map[string]any{"self": h.Cfg.HostName, "hosts": n.cfg.Hosts})
}

// ---- control ----

func (h *Hub) hRotateControl(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	var req struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !proto.ValidToken(req.Token) {
		httpErr(w, 400, "new control token must be 64 hexadecimal characters")
		return
	}

	n.mu.Lock()
	next := n.cfg
	if req.Token == next.ControlToken && next.ControlHost == h.Cfg.HostName {
		n.mu.Unlock()
		httpErr(w, 400, "new control token is unchanged")
		return
	}
	next.ControlToken = req.Token
	next.ControlHost = h.Cfg.HostName
	if err := config.SaveNet(next); err != nil {
		n.mu.Unlock()
		httpErr(w, 500, "save: %v", err)
		return
	}
	n.cfg = next
	n.mu.Unlock()

	n.auditLine(h.actor(r, id), "rotate-control", h.Cfg.HostName, "new token is host-local")
	writeJSON(w, 200, map[string]string{"control_host": h.Cfg.HostName})
}

type spawnReq struct {
	Name    string   `json:"name"`
	Cmd     []string `json:"cmd"`
	Cwd     string   `json:"cwd,omitempty"`
	Profile string   `json:"profile,omitempty"`  // spawn profile: context + MCP provisioning
	SSHHost string   `json:"ssh_host,omitempty"` // origin-side: forward this spawn to an SSH host's hub
	// Provision is a resolved provisioning spec carried on a forwarded spawn
	// (the remote hub has no access to the origin's profile files). Internal —
	// clients set Profile, not this.
	Provision    *provisionSpec `json:"provision,omitempty"`
	GrantControl bool           `json:"grant_control,omitempty"`
	Nudge        bool           `json:"nudge,omitempty"` // opt into fixed terminal wake notices
	WaitReady    bool           `json:"wait_ready,omitempty"`
	Headed       bool           `json:"headed,omitempty"`  // open a visible terminal window attached to the session
	Persist      bool           `json:"persist,omitempty"` // declare it: the daemon respawns it after reboot/crash
}

type spawnResp struct {
	Agent       string `json:"agent"`
	Session     string `json:"session"`
	Pane        string `json:"pane"`
	Nudge       bool   `json:"nudge,omitempty"`
	NudgePolicy string `json:"nudge_policy"`
	Ready       bool   `json:"ready"`
	State       string `json:"state,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Transcript  string `json:"transcript,omitempty"`
	Window      string `json:"window,omitempty"` // headed result: "opened" or the error
}

func (h *Hub) hSpawn(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	var req spawnReq
	if !readJSON(w, r, &req) {
		return
	}
	if !proto.ValidName(req.Name) {
		httpErr(w, 400, "bad agent name")
		return
	}

	// SSH-host target: bring the host up (transient daemon + tunnels) and
	// forward a normal spawn to its hub. Handled entirely here; returns.
	if req.SSHHost != "" {
		h.spawnOntoSSH(w, r, n, id, req)
		return
	}

	// Resolve the spawn profile (context + MCP provisioning). A forwarded spawn
	// already carries a resolved spec (req.Provision) and skips profile lookup,
	// since the profile files live on the origin, not here.
	var prof config.SpawnProfile
	if req.Provision == nil && req.Profile != "" {
		var err error
		if prof, err = config.LoadProfile(req.Profile); err != nil {
			httpErr(w, 400, "profile %q: %v", req.Profile, err)
			return
		}
	}
	if len(req.Cmd) == 0 {
		req.Cmd = prof.Runtime
	}
	if len(req.Cmd) == 0 {
		httpErr(w, 400, "empty command (give a `-- CMD` or a profile with a runtime)")
		return
	}
	if req.Cwd == "" {
		req.Cwd = prof.Cwd
	}
	req.Cwd = expandHome(req.Cwd)

	spec := provisionSpec{}
	if req.Provision != nil {
		spec = *req.Provision
	} else {
		var err error
		if spec, err = buildProvision(prof); err != nil {
			httpErr(w, 500, "provision: %v", err)
			return
		}
	}

	// Provision the working directory (context files, .mcp.json, trust) before
	// the runtime starts. Only when there is a cwd to write into — without one
	// there is no project dir to seed, so a bare `spawn -- claude` is unchanged.
	if req.Cwd != "" {
		if err := applyProvision(req.Cwd, spec, hiveBinPath()); err != nil {
			httpErr(w, 500, "provision: %v", err)
			return
		}
	}
	if spec.Sandbox != nil {
		var err error
		req.Cmd, err = wrapSandboxCommand(*spec.Sandbox, req.Name, req.Cwd, req.Cmd)
		if err != nil {
			httpErr(w, 400, "sandbox: %v", err)
			return
		}
	}

	session := "hive-" + n.name + "-" + req.Name

	tok, env, err := h.spawnEnv(n, req)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}

	rec, serr := h.spawnCore(n, h.actor(r, id), req, session, tok, env)
	if serr != nil {
		// A persist-spawn of an agent that is already alive is a declaration
		// update, not a conflict — that makes `spawn --persist` idempotent
		// ("ensure declared and running"), e.g. on re-provisioning.
		if serr.code == 409 && req.Persist {
			n.regMu.Lock()
			old, ok := n.reg.Get(req.Name)
			if ok && alive(old) && old.Session == session {
				if old.Nudge != req.Nudge {
					n.regMu.Unlock()
					httpErr(w, 409, "agent %q is already live with nudge=%v; kill it before changing terminal-wake policy", req.Name, old.Nudge)
					return
				}
				if err := h.declare(n, req); err != nil {
					n.regMu.Unlock()
					httpErr(w, 500, "persist: %v", err)
					return
				}
				n.regMu.Unlock()
				n.auditLine(h.actor(r, id), "spawn", req.Name+"@"+h.Cfg.HostName, "already live; declaration updated")
				writeJSON(w, 200, spawnResp{
					Agent: old.Name + "@" + h.Cfg.HostName, Session: old.Session, Pane: old.Pane,
					Nudge: old.Nudge, NudgePolicy: "explicit", Ready: true, Transcript: old.Transcript,
				})
				return
			}
			n.regMu.Unlock()
		}
		httpErr(w, serr.code, "%s", serr.msg)
		return
	}
	if req.Persist {
		if err := h.declare(n, req); err != nil {
			// The spawn itself succeeded; a failed declaration must not
			// silently pass for one.
			httpErr(w, 500, "spawned, but could not persist the declaration: %v", err)
			return
		}
	}
	window := ""
	if req.Headed {
		// Best-effort: a spawn without a visible window is still a
		// working spawn.
		if err := control.OpenWindow(session, rec.Pane); err != nil {
			window = "error: " + err.Error()
		} else {
			window = "opened"
		}
	}
	ready := false
	state := "started"
	detail := ""
	if req.WaitReady {
		// No pre-sleep: WaitQuiescent's first capture/window/compare cycle
		// already tolerates a not-yet-drawn frame (a still-drawing pane is
		// simply non-quiescent and gets re-polled), so a fixed 500ms up
		// front was pure dead time on every wait-ready spawn.
		ready = control.WaitQuiescent(rec.Pane, 700*time.Millisecond, 15*time.Second)
		state, detail = waitSemanticReadiness(rec.Pane, ready, 4*time.Second)
		ready = state == "ready"
	}
	writeJSON(w, 200, spawnResp{
		Agent: rec.Name + "@" + h.Cfg.HostName, Session: rec.Session, Pane: rec.Pane,
		Nudge: rec.Nudge, NudgePolicy: "explicit", Ready: ready, State: state, Detail: detail,
		Transcript: rec.Transcript, Window: window,
	})
}

func waitSemanticReadiness(pane string, quiescent bool, timeout time.Duration) (string, string) {
	if !quiescent {
		return classifySpawnReadiness(pane, false)
	}
	deadline := time.Now().Add(timeout)
	for {
		state, detail := classifySpawnReadiness(pane, true)
		if state != "starting" || time.Now().After(deadline) {
			return state, detail
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func classifySpawnReadiness(pane string, quiescent bool) (string, string) {
	if !control.PaneExists(pane) {
		return "exited", "pane_closed"
	}
	if !quiescent {
		return "starting", "terminal_not_quiescent"
	}
	screen, err := control.Capture(pane, 80)
	if err != nil {
		return "starting", "capture_failed"
	}
	if strings.TrimSpace(screen) == "" {
		return "starting", "terminal_empty"
	}
	if detail := classifyRuntimePrompt(screen); detail != "" {
		return "blocked_on_runtime_prompt", detail
	}
	return "ready", ""
}

func classifyRuntimePrompt(screen string) string {
	lower := strings.ToLower(screen)
	blockers := []struct {
		needle string
		detail string
	}{
		{"update available!", "runtime_update_prompt"},
		{"do you trust the contents of this directory", "workspace_trust_prompt"},
		{"is this a project you created or one you trust", "workspace_trust_prompt"},
		{"choose the text style that looks best", "runtime_theme_prompt"},
		{"select login method", "runtime_login_prompt"},
		{"new mcp server found in this project", "mcp_approval_prompt"},
		{"bypass permissions mode", "permission_bypass_prompt"},
		{"opening browser to sign in", "runtime_login_prompt"},
		{"paste code here if prompted", "runtime_login_prompt"},
	}
	for _, blocker := range blockers {
		if strings.Contains(lower, blocker.needle) {
			return blocker.detail
		}
	}
	return ""
}

func wrapSandboxCommand(s config.SandboxRunner, agent, workspace string, command []string) ([]string, error) {
	if !filepath.IsAbs(s.Command) || filepath.Clean(s.Command) != s.Command {
		return nil, fmt.Errorf("command must be a clean absolute path")
	}
	if !filepath.IsAbs(s.Profiles) || filepath.Clean(s.Profiles) != s.Profiles {
		return nil, fmt.Errorf("profiles must be a clean absolute path")
	}
	if !proto.ValidName(s.Profile) {
		return nil, fmt.Errorf("profile name is invalid")
	}
	if workspace == "" {
		return nil, fmt.Errorf("a sandboxed spawn requires cwd")
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace")
	}
	wrapped := []string{
		s.Command, "run", "--profiles", s.Profiles, "--profile", s.Profile,
		"--agent", agent, "--workspace", filepath.Clean(workspace), "--",
	}
	return append(wrapped, command...), nil
}

// declare records a spawn request as a desired session in the persist store.
func (h *Hub) declare(n *network, req spawnReq) error {
	return n.persist.Put(store.PersistSpec{
		Name: req.Name, Cmd: req.Cmd, Cwd: req.Cwd,
		GrantControl: req.GrantControl, Nudge: req.Nudge, Declared: time.Now().UnixMilli(),
	})
}

// spawnEnv mints the agent token and builds the spawn environment. It fails
// only when control is requested and this host has no control token to grant.
func (h *Hub) spawnEnv(n *network, req spawnReq) (string, map[string]string, error) {
	tok := proto.NewToken()
	bindHost := h.Cfg.Bind
	if bindHost == "0.0.0.0" || bindHost == "::" || bindHost == "" {
		bindHost = "127.0.0.1"
	}
	env := map[string]string{
		"HIVE_ADDR":  fmt.Sprintf("http://%s", net.JoinHostPort(bindHost, strconv.Itoa(h.Cfg.Port))),
		"HIVE_NET":   n.name,
		"HIVE_AGENT": req.Name + "@" + h.Cfg.HostName,
		"HIVE_TOKEN": tok,
	}
	if req.GrantControl {
		n.mu.Lock()
		ctl := n.cfg.ControlFor(h.Cfg.HostName)
		ctlHost := n.cfg.ControlHost
		n.mu.Unlock()
		if ctl == "" {
			return "", nil, fmt.Errorf("this host has no control token to grant")
		}
		env["HIVE_CONTROL_TOKEN"] = ctl
		if ctlHost != "" {
			env["HIVE_CONTROL_HOST"] = ctlHost
		}
	}
	return tok, env, nil
}

// spawnErr is a spawn failure with the HTTP status it maps to, so the
// reconcile loop can share spawnCore without an http.ResponseWriter.
type spawnErr struct {
	code int
	msg  string
}

func (e *spawnErr) Error() string { return e.msg }

// spawnCore atomically claims the agent name and creates its tmux
// session + registry record, serialized by n.regMu against other
// register/spawn calls.
func (h *Hub) spawnCore(n *network, actor string,
	req spawnReq, session, tok string, env map[string]string) (store.AgentRec, *spawnErr) {
	n.regMu.Lock()
	defer n.regMu.Unlock()
	if old, ok := n.reg.Get(req.Name); ok && alive(old) {
		return store.AgentRec{}, &spawnErr{409, fmt.Sprintf("name %q is taken by a live agent", req.Name)}
	}
	pane, pid, err := control.NewSession(session, req.Cwd, env, req.Cmd, req.Headed)
	if errors.Is(err, control.ErrDuplicateSession) {
		// Reclaim only a session this network's registry owns (a dead
		// registration or crash leftover). tmux's session namespace is
		// flat, so an unowned name may belong to another network or to
		// something the user started — never kill those. (Windows spawns
		// have no shared namespace and never collide.)
		if old, ok := n.reg.Get(req.Name); ok && old.Session == session {
			control.KillSession(session, old.Pane)
			pane, pid, err = control.NewSession(session, req.Cwd, env, req.Cmd, req.Headed)
		} else {
			return store.AgentRec{}, &spawnErr{409,
				fmt.Sprintf("tmux session %q exists but is not owned by network %q — kill it manually", session, n.name)}
		}
	}
	if err != nil {
		return store.AgentRec{}, &spawnErr{500, fmt.Sprintf("spawn: %v", err)}
	}
	epoch, err := control.ProcStartEpoch(pid)
	if err != nil {
		control.KillSession(session, pane)
		return store.AgentRec{}, &spawnErr{500, "spawned process died immediately"}
	}
	if old, ok := n.reg.Get(req.Name); ok && old.Ephemeral {
		if err := n.retireInbox(req.Name); err != nil {
			control.KillSession(session, pane)
			return store.AgentRec{}, &spawnErr{500, fmt.Sprintf("retire expired mailbox: %v", err)}
		}
	}
	rec := store.AgentRec{
		Name: req.Name, TokenHash: proto.HashToken(tok),
		Pane: pane, Session: session, PID: pid, StartEpoch: epoch,
		Spawned: true, Nudge: req.Nudge, Registered: time.Now().UnixMilli(),
	}
	if transcript, err := startTranscript(n.name, req.Name, pane); err == nil {
		rec.Transcript = transcript
	} else {
		n.auditLine(actor, "transcript", rec.Name+"@"+h.Cfg.HostName, "disabled: "+err.Error())
	}
	if err := n.reg.Put(rec); err != nil {
		control.KillSession(session, pane)
		return store.AgentRec{}, &spawnErr{500, fmt.Sprintf("registry: %v", err)}
	}
	n.auditLine(actor, "spawn", rec.Name+"@"+h.Cfg.HostName,
		fmt.Sprintf("cmd=%q grant_control=%v nudge=%v headed=%v persist=%v", strings.Join(req.Cmd, " "), req.GrantControl, req.Nudge, req.Headed, req.Persist))
	return rec, nil
}

func startTranscript(network, agent, pane string) (string, error) {
	dir := filepath.Join(config.Home(), "runs", network)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.log", agent, time.Now().UTC().Format("20060102T150405.000000000Z")))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := control.StartCapture(pane, path); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// localControllable resolves an agent arg (name or name@thishost) to a
// live, pane-bound record or writes the appropriate error.
func (h *Hub) localControllable(w http.ResponseWriter, n *network, agent string) (store.AgentRec, bool) {
	name := agent
	if strings.Contains(agent, "@") {
		var host string
		var err error
		name, host, err = proto.SplitAgent(agent)
		if err != nil {
			httpErr(w, 400, "%v", err)
			return store.AgentRec{}, false
		}
		if host != h.Cfg.HostName {
			httpErr(w, 400, "misrouted: %s is not on host %s (control ops go to the target hub)", agent, h.Cfg.HostName)
			return store.AgentRec{}, false
		}
	}
	rec, ok := n.reg.Get(name)
	if !ok {
		httpErr(w, 404, "no such agent")
		return store.AgentRec{}, false
	}
	if rec.Pane == "" {
		httpErr(w, 409, "agent %s is not controllable (no tmux pane bound)", name)
		return store.AgentRec{}, false
	}
	if !alive(rec) {
		httpErr(w, 409, "agent %s is gone (pane closed or process replaced)", name)
		return store.AgentRec{}, false
	}
	return rec, true
}

func (h *Hub) hKeys(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	var req struct {
		Agent string `json:"agent"`
		Text  string `json:"text"`
		Enter bool   `json:"enter,omitempty"`
		Raw   bool   `json:"raw,omitempty"` // terminal input: never bracketed-paste
	}
	if !readJSON(w, r, &req) {
		return
	}
	rec, ok := h.localControllable(w, n, req.Agent)
	if !ok {
		return
	}
	var err error
	if req.Text != "" {
		if !req.Raw && strings.ContainsAny(req.Text, "\n\r") {
			err = control.Paste(rec.Pane, req.Text)
		} else {
			err = control.SendKeysLiteral(rec.Pane, req.Text)
		}
	}
	submitted := 0
	if err == nil && req.Enter {
		if req.Raw || req.Text == "" {
			err = control.Enter(rec.Pane)
		} else {
			// Verified submit: an Enter sent while the TUI is still digesting
			// the text can be swallowed, leaving it parked unsubmitted in the
			// input box while the agent idles. Give the TUI a beat, then
			// retry while the text still sits on the input line.
			submitted, err = verifiedEnter(rec.Pane, req.Text)
		}
	}
	if err != nil {
		httpErr(w, 500, "keys: %v", err)
		return
	}
	n.auditLine(h.actor(r, id), "keys", rec.Name+"@"+h.Cfg.HostName,
		fmt.Sprintf("%dB enter=%v attempts=%d", len(req.Text), req.Enter, submitted))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// verifiedEnter submits typed text and confirms it left the input line,
// retrying while it sits parked. The parked check only recognizes a
// "❯"-prompt input box (Claude Code style); any other TUI gets exactly one
// Enter, unverified, as before. Persistent parking is an error — the caller
// must not mistake a parked directive for a delivered one.
func verifiedEnter(pane, text string) (int, error) {
	time.Sleep(700 * time.Millisecond)
	for attempt := 1; ; attempt++ {
		if err := control.Enter(pane); err != nil {
			return attempt, err
		}
		time.Sleep(1500 * time.Millisecond)
		cap, err := control.Capture(pane, 0)
		if err != nil || !keysParked(cap, text) {
			return attempt, nil
		}
		if attempt >= 4 {
			return attempt, fmt.Errorf("text parked unsubmitted after %d Enter attempts (pane input wedged)", attempt)
		}
		time.Sleep(time.Duration(attempt) * time.Second)
	}
}

// keysParked reports whether text still sits unsubmitted on the pane's input
// line: the LAST line starting with "❯" is the input box; submitted text
// never re-echoes there.
func keysParked(pane, text string) bool {
	first := text
	if i := strings.IndexAny(first, "\r\n"); i >= 0 {
		first = first[:i]
	}
	if len(first) > 40 {
		first = first[:40]
	}
	if first == "" {
		return false
	}
	prompt := ""
	for _, ln := range strings.Split(pane, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "❯") {
			prompt = ln
		}
	}
	return prompt != "" && strings.Contains(prompt, first)
}

func (h *Hub) hRead(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	rec, ok := h.localControllable(w, n, r.URL.Query().Get("agent"))
	if !ok {
		return
	}
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	out, err := control.Capture(rec.Pane, lines)
	if err != nil {
		httpErr(w, 500, "read: %v", err)
		return
	}
	n.auditLine(h.actor(r, id), "read", rec.Name+"@"+h.Cfg.HostName, fmt.Sprintf("lines=%d", lines))
	writeJSON(w, 200, map[string]any{"agent": rec.Name + "@" + h.Cfg.HostName, "screen": out})
}

func (h *Hub) hKill(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	var req struct {
		Agent  string `json:"agent"`
		Forget bool   `json:"forget,omitempty"` // also drop its persist declaration
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSuffix(req.Agent, "@"+h.Cfg.HostName)
	// Drop the declaration BEFORE killing: the reconciler must not race the
	// kill and respawn what the caller is tearing down. A plain kill leaves
	// the declaration, so a declared agent comes back on the next sweep.
	forgotten := false
	hadDeclaration := false
	if req.Forget {
		_, hadDeclaration = n.persist.Get(name)
		if err := n.persist.Delete(name); err != nil {
			httpErr(w, 500, "persist: %v", err)
			return
		}
		forgotten = true
	}

	// Atomically detach the exact registration before the potentially slow
	// external teardown. Registration and spawn claims use the same regMu, so
	// no stale Delete can land after a replacement has claimed the reusable
	// name. Releasing the lock before KillSession also lets a pane-less claimant
	// join immediately; a spawned claimant safely waits for the old session
	// name to disappear from the control backend.
	n.regMu.Lock()
	rec, ok := n.reg.Get(name)
	if !ok {
		n.regMu.Unlock()
		// A declared-but-dead session has no registry record; forgetting it
		// must still work, else the reconciler resurrects it forever.
		if forgotten && hadDeclaration {
			n.auditLine(h.actor(r, id), "kill", name+"@"+h.Cfg.HostName, "forgot declaration (no live agent)")
			writeJSON(w, 200, map[string]any{"killed": false, "deregistered": false, "forgotten": true})
			return
		}
		httpErr(w, 404, "no such agent")
		return
	}
	if rec.Ephemeral {
		if err := n.retireInbox(name); err != nil {
			n.regMu.Unlock()
			httpErr(w, 500, "retire ephemeral mailbox: %v", err)
			return
		}
	}
	if err := n.reg.Delete(name); err != nil {
		n.regMu.Unlock()
		httpErr(w, 500, "registry: %v", err)
		return
	}
	n.regMu.Unlock()

	killed := false
	if rec.Spawned && rec.Session != "" {
		if err := h.killSession(rec.Session, rec.Pane); err == nil {
			killed = true
		}
	}
	n.auditLine(h.actor(r, id), "kill", name+"@"+h.Cfg.HostName, fmt.Sprintf("killed_session=%v forgotten=%v", killed, forgotten))
	writeJSON(w, 200, map[string]any{"killed": killed, "deregistered": true, "forgotten": forgotten})
}

// ---- hub-to-hub ----

// rpc performs one JSON call against another hub.
func (h *Hub) rpc(method, addr, path, token string, in, out any) error {
	var body *strings.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(b))
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequest(method, "http://"+addr+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e errResp
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

// forwardDeliver pushes an envelope into a remote hub's local inboxes.
func (h *Hub) forwardDeliver(addr, netName, msgTok string, env proto.Envelope) (map[string]string, error) {
	var out struct {
		Results map[string]string `json:"results"`
	}
	err := h.rpc("POST", addr, "/v1/nets/"+netName+"/deliver", msgTok, deliverReq{Env: env}, &out)
	if err != nil {
		return nil, err
	}
	return out.Results, nil
}
