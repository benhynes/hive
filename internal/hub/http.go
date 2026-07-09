package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
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

// maxWait caps long-poll hold time per request.
const maxWait = 25 * time.Second

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
		id, ok := n.resolve(bearer(r))
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
		writeJSON(w, 200, map[string]any{"api": "hive", "v": 1, "host": h.Cfg.HostName})
	})
	mux.HandleFunc("POST /v1/nets/{net}/register", h.withNet(accNetTok, h.hRegister))
	mux.HandleFunc("POST /v1/nets/{net}/deregister", h.withNet(accAny, h.hDeregister))
	mux.HandleFunc("GET /v1/nets/{net}/agents", h.withNet(accAny, h.hAgents))
	mux.HandleFunc("POST /v1/nets/{net}/send", h.withNet(accAny, h.hSend))
	mux.HandleFunc("POST /v1/nets/{net}/deliver", h.withNet(accNetTok, h.hDeliver))
	mux.HandleFunc("GET /v1/nets/{net}/inbox", h.withNet(accAny, h.hInbox))
	mux.HandleFunc("POST /v1/nets/{net}/ack", h.withNet(accAny, h.hAck))
	mux.HandleFunc("GET /v1/nets/{net}/hosts", h.withNet(accAny, h.hHostsGet))
	mux.HandleFunc("POST /v1/nets/{net}/hosts", h.withNet(accControl, h.hHostsPost))
	mux.HandleFunc("POST /v1/nets/{net}/spawn", h.withNet(accControl, h.hSpawn))
	mux.HandleFunc("POST /v1/nets/{net}/keys", h.withNet(accControl, h.hKeys))
	mux.HandleFunc("GET /v1/nets/{net}/read", h.withNet(accControl, h.hRead))
	mux.HandleFunc("GET /v1/nets/{net}/stream", h.withNet(accControl, h.hStream))
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
	Name string `json:"name"`
	Pane string `json:"pane,omitempty"` // caller's $TMUX_PANE, verified here
	PID  int    `json:"pid,omitempty"`  // fallback liveness binding
}

type registerResp struct {
	Agent string `json:"agent"`
	Token string `json:"token"`
}

func (h *Hub) hRegister(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	var req registerReq
	if !readJSON(w, r, &req) {
		return
	}
	if !proto.ValidName(req.Name) {
		httpErr(w, 400, "bad agent name (want [a-z0-9][a-z0-9_-]*, ≤32)")
		return
	}
	n.regMu.Lock()
	defer n.regMu.Unlock()
	if old, ok := n.reg.Get(req.Name); ok && alive(old) {
		httpErr(w, 409, "name %q is taken by a live agent", req.Name)
		return
	}
	rec := store.AgentRec{Name: req.Name, Registered: time.Now().UnixMilli()}
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
	tok := proto.NewToken()
	rec.TokenHash = proto.HashToken(tok)
	if err := n.reg.Put(rec); err != nil {
		httpErr(w, 500, "registry: %v", err)
		return
	}
	n.auditLine(h.actor(r, id), "register", req.Name+"@"+h.Cfg.HostName, "")
	writeJSON(w, 200, registerResp{Agent: req.Name + "@" + h.Cfg.HostName, Token: tok})
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
	if _, ok := n.reg.Get(name); !ok {
		httpErr(w, 404, "no such agent")
		return
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
	Spawned      bool   `json:"spawned,omitempty"`
	Registered   int64  `json:"registered"`
}

type agentsResp struct {
	Agents      []agentInfo       `json:"agents"`
	Unreachable map[string]string `json:"unreachable,omitempty"`
}

func (h *Hub) localAgents(n *network) []agentInfo {
	var out []agentInfo
	for _, rec := range n.reg.List() {
		out = append(out, agentInfo{
			Agent:        rec.Name + "@" + h.Cfg.HostName,
			Alive:        alive(rec),
			Controllable: rec.Pane != "",
			Spawned:      rec.Spawned,
			Registered:   rec.Registered,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

func (h *Hub) hAgents(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	resp := agentsResp{Agents: h.localAgents(n), Unreachable: map[string]string{}}
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
	if _, ok := n.reg.Get(agent); !ok {
		httpErr(w, 404, "no such agent")
		return
	}
	ib, err := n.inbox(agent)
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
	ib, err := n.inbox(id.Agent)
	if err != nil {
		httpErr(w, 500, "inbox: %v", err)
		return
	}
	if err := ib.Ack(req.Seq); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]any{"cursor": ib.Cursor()})
}

// ---- hosts ----

func (h *Hub) hHostsGet(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	writeJSON(w, 200, map[string]any{"self": h.Cfg.HostName, "hosts": n.hosts()})
}

func (h *Hub) hHostsPost(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	var req struct {
		Op   string `json:"op"`
		Name string `json:"name"`
		Addr string `json:"addr,omitempty"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !proto.ValidName(req.Name) {
		httpErr(w, 400, "bad host name")
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
		httpErr(w, 400, "op must be add or rm")
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

type spawnReq struct {
	Name         string   `json:"name"`
	Cmd          []string `json:"cmd"`
	Cwd          string   `json:"cwd,omitempty"`
	GrantControl bool     `json:"grant_control,omitempty"`
	WaitReady    bool     `json:"wait_ready,omitempty"`
	Headed       bool     `json:"headed,omitempty"`  // open a visible terminal window attached to the session
	Persist      bool     `json:"persist,omitempty"` // declare it: the daemon respawns it after reboot/crash
}

type spawnResp struct {
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Pane    string `json:"pane"`
	Ready   bool   `json:"ready"`
	Window  string `json:"window,omitempty"` // headed result: "opened" or the error
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
	if len(req.Cmd) == 0 {
		httpErr(w, 400, "empty command")
		return
	}
	session := "hive-" + n.name + "-" + req.Name

	tok, env, err := h.spawnEnv(n, req)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}

	rec, serr := h.spawnCore(n, h.actor(r, id), req, session, tok, env)
	if serr != nil {
		httpErr(w, serr.code, "%s", serr.msg)
		return
	}
	if req.Persist {
		spec := store.PersistSpec{
			Name: req.Name, Cmd: req.Cmd, Cwd: req.Cwd,
			GrantControl: req.GrantControl, Declared: time.Now().UnixMilli(),
		}
		if err := n.persist.Put(spec); err != nil {
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
	if req.WaitReady {
		// No pre-sleep: WaitQuiescent's first capture/window/compare cycle
		// already tolerates a not-yet-drawn frame (a still-drawing pane is
		// simply non-quiescent and gets re-polled), so a fixed 500ms up
		// front was pure dead time on every wait-ready spawn.
		ready = control.WaitQuiescent(rec.Pane, 700*time.Millisecond, 15*time.Second)
	}
	writeJSON(w, 200, spawnResp{
		Agent: rec.Name + "@" + h.Cfg.HostName, Session: rec.Session, Pane: rec.Pane,
		Ready: ready, Window: window,
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
		ctl := n.cfg.ControlToken
		n.mu.Unlock()
		if ctl == "" {
			return "", nil, fmt.Errorf("this host has no control token to grant")
		}
		env["HIVE_CONTROL_TOKEN"] = ctl
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
	rec := store.AgentRec{
		Name: req.Name, TokenHash: proto.HashToken(tok),
		Pane: pane, Session: session, PID: pid, StartEpoch: epoch,
		Spawned: true, Registered: time.Now().UnixMilli(),
	}
	if err := n.reg.Put(rec); err != nil {
		control.KillSession(session, pane)
		return store.AgentRec{}, &spawnErr{500, fmt.Sprintf("registry: %v", err)}
	}
	n.auditLine(actor, "spawn", rec.Name+"@"+h.Cfg.HostName,
		fmt.Sprintf("cmd=%q grant_control=%v headed=%v persist=%v", strings.Join(req.Cmd, " "), req.GrantControl, req.Headed, req.Persist))
	return rec, nil
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
	if err == nil && req.Enter {
		err = control.Enter(rec.Pane)
	}
	if err != nil {
		httpErr(w, 500, "keys: %v", err)
		return
	}
	n.auditLine(h.actor(r, id), "keys", rec.Name+"@"+h.Cfg.HostName,
		fmt.Sprintf("%dB enter=%v", len(req.Text), req.Enter))
	writeJSON(w, 200, map[string]any{"ok": true})
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

// hStream sends the pane's screen (escape sequences + cursor position),
// then live raw output until the client disconnects or the pane dies.
// Pane geometry rides in headers so a terminal emulator can size itself
// before the first byte. One tmux pipe-pane feeds all concurrent
// streams; see internal/hub/stream.go.
func (h *Hub) hStream(w http.ResponseWriter, r *http.Request, n *network, id ident) {
	if !control.StreamSupported() {
		httpErr(w, 501, "pane streaming is not supported on this host")
		return
	}
	rec, ok := h.localControllable(w, n, r.URL.Query().Get("agent"))
	if !ok {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		httpErr(w, 500, "streaming unsupported by server")
		return
	}
	cols, rows, err := control.PaneSize(rec.Pane)
	if err != nil {
		httpErr(w, 500, "pane size: %v", err)
		return
	}
	// Subscribe before the snapshot: output between the two is then
	// delivered rather than lost (duplicated bytes just redraw).
	ch, cancel, err := h.streams.Subscribe(rec.Pane)
	if err != nil {
		httpErr(w, 500, "stream: %v", err)
		return
	}
	defer cancel()
	snap, err := control.CaptureRaw(rec.Pane)
	if err != nil {
		httpErr(w, 500, "capture: %v", err)
		return
	}
	n.auditLine(h.actor(r, id), "stream", rec.Name+"@"+h.Cfg.HostName, "open")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Hive-Cols", strconv.Itoa(cols))
	w.Header().Set("X-Hive-Rows", strconv.Itoa(rows))
	w.WriteHeader(200)
	if _, err := w.Write([]byte(snap)); err != nil {
		return
	}
	fl.Flush()
	ctx := r.Context()
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return // pane died or we fell behind; client reconnects
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			fl.Flush()
		case <-ctx.Done():
			return
		}
	}
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
	rec, ok := n.reg.Get(name)
	if !ok {
		// A declared-but-dead session has no registry record; forgetting it
		// must still work, else the reconciler resurrects it forever.
		if req.Forget {
			if _, declared := n.persist.Get(name); declared {
				if err := n.persist.Delete(name); err != nil {
					httpErr(w, 500, "persist: %v", err)
					return
				}
				n.auditLine(h.actor(r, id), "kill", name+"@"+h.Cfg.HostName, "forgot declaration (no live agent)")
				writeJSON(w, 200, map[string]any{"killed": false, "deregistered": false, "forgotten": true})
				return
			}
		}
		httpErr(w, 404, "no such agent")
		return
	}
	// Drop the declaration BEFORE killing: the reconciler must not race the
	// kill and respawn what the caller is tearing down. A plain kill leaves
	// the declaration, so a declared agent comes back on the next sweep.
	forgotten := false
	if req.Forget {
		if err := n.persist.Delete(name); err != nil {
			httpErr(w, 500, "persist: %v", err)
			return
		}
		forgotten = true
	}
	killed := false
	if rec.Spawned && rec.Session != "" {
		if err := control.KillSession(rec.Session, rec.Pane); err == nil {
			killed = true
		}
	}
	if err := n.reg.Delete(name); err != nil {
		httpErr(w, 500, "registry: %v", err)
		return
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
