// dash.go — `hive dash`: a local web dashboard over the control layer.
//
// The dash process is a poller + cache + static page: it discovers agents
// through the registry (so tiles appear/disappear as workers spawn and
// die), captures each controllable agent's screen via the read endpoint
// (never attaches, so agents keep their native pane size), derives a
// status per agent from the captured text, and serves one embedded HTML
// page that polls /api/state. Interaction (keystrokes, kill) goes through
// the existing keys/kill endpoints — no VM-side changes needed.
//
// Security: binds loopback by default; every /api call must carry the
// per-run random token embedded in the served page (blocks cross-origin
// drive-by POSTs), and the Host header must match the bind address
// (blocks DNS rebinding).
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/benhynes/hive/internal/client"
	"github.com/benhynes/hive/internal/proto"
)

//go:embed dash.html
var dashHTML string

func runDash(args []string) error {
	fs := flags("dash", args)
	_ = fs.Bool("web", true, "serve the web UI (the only mode)")
	bind := fs.String("bind", "127.0.0.1", "address to serve the dashboard on")
	port := fs.Int("port", 7780, "dashboard port")
	open := fs.Bool("open", true, "open the dashboard in the default browser")
	lines := fs.Int("lines", 0, "scrollback lines to include in each capture (0 = visible pane)")
	fs.Parse2()

	// Loopback only: the served page embeds the API token, and the API
	// fronts the net's control token (keys/kill on every agent). A
	// non-loopback bind would hand mesh control to anyone who can reach
	// the port. View remotely via: ssh -L 7780:127.0.0.1:7780 <host>.
	if ip := net.ParseIP(*bind); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("dash only binds loopback addresses (got %q) — for remote viewing use: ssh -L %d:127.0.0.1:%d <this-host>", *bind, *port, *port)
	}

	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	if c.Control == "" {
		return fmt.Errorf("dash needs the control token (hold it in net.json or set HIVE_CONTROL_TOKEN)")
	}
	// Attribute dash's polling in every hub's audit log.
	if c.Agent == "" {
		c.Agent = "dash@local"
	}
	c.SetHTTPTimeout(8 * time.Second)
	// Populate the hosts cache before the poll loops start; the agents
	// loop refreshes it so hosts added while dash runs are picked up.
	if _, err := c.Hosts(); err != nil {
		return fmt.Errorf("local hub unreachable at %s: %w", c.Addr, err)
	}

	d := newDash(c, *lines)
	go d.agentsLoop()
	go d.screensLoop()

	addr := net.JoinHostPort(*bind, strconv.Itoa(*port))
	srv := &http.Server{Addr: addr, Handler: d.handler(addr)}
	url := "http://" + addr + "/"
	fmt.Printf("hive dash: net=%s serving %s\n", c.Net, url)
	if *open && runtime.GOOS == "darwin" {
		exec.Command("open", url).Start()
	}
	return srv.ListenAndServe()
}

type paneState struct {
	Screen    string
	Err       string
	FetchedAt time.Time
	ChangedAt time.Time
}

type dash struct {
	c     *client.Client
	token string // per-run API token embedded in the page
	lines int

	mu          sync.Mutex
	agents      []client.AgentInfo
	unreachable map[string]string
	agentsErr   string
	panes       map[string]*paneState // by full agent id
}

func newDash(c *client.Client, lines int) *dash {
	b := make([]byte, 16)
	rand.Read(b)
	return &dash{c: c, token: hex.EncodeToString(b), lines: lines,
		panes: map[string]*paneState{}, unreachable: map[string]string{}}
}

// agentsLoop refreshes the mesh-wide agent list (and the hosts cache, so
// reads can reach hosts added after dash started).
func (d *dash) agentsLoop() {
	for ; ; time.Sleep(5 * time.Second) {
		res, err := d.c.Agents(false)
		d.c.Hosts()
		d.mu.Lock()
		if err != nil {
			d.agentsErr = err.Error()
		} else {
			d.agentsErr = ""
			d.agents = res.Agents
			d.unreachable = res.Unreachable
			if d.unreachable == nil {
				d.unreachable = map[string]string{}
			}
			// Drop pane state for agents that left the registry.
			known := map[string]bool{}
			for _, a := range res.Agents {
				known[a.Agent] = true
			}
			for id := range d.panes {
				if !known[id] {
					delete(d.panes, id)
				}
			}
		}
		d.mu.Unlock()
	}
}

// screensLoop captures live controllable agents' panes. Capture never
// attaches a client, so it cannot resize the pane. Each tick dispatches
// fire-and-forget captures and never waits for stragglers: an agent with
// a capture still in flight (black-holed host) is skipped, so one slow
// host can't stall the refresh of healthy ones. Long-quiet agents are
// polled on a slower tier — mostly to keep dash from flooding every
// hub's audit log (each read is one audit line) when the fleet is idle.
func (d *dash) screensLoop() {
	inflight := map[string]bool{} // guarded by d.mu
	for tick := 0; ; tick++ {
		now := time.Now()
		d.mu.Lock()
		var targets []string
		for _, a := range d.agents {
			if !a.Alive || !a.Controllable || inflight[a.Agent] {
				continue
			}
			// Tiers by time since the screen last changed:
			// <60s → every tick (1.5s); <5m → every 4th (6s);
			// else every 10th (15s). A dialog appearing on a busy
			// agent is caught fast (busy = fast tier already).
			if p := d.panes[a.Agent]; p != nil && !p.FetchedAt.IsZero() && p.Err == "" {
				quiet := now.Sub(p.ChangedAt)
				if quiet >= 5*time.Minute && tick%10 != 0 {
					continue
				}
				if quiet >= time.Minute && tick%4 != 0 {
					continue
				}
			}
			targets = append(targets, a.Agent)
			inflight[a.Agent] = true
		}
		d.mu.Unlock()
		for _, id := range targets {
			go func(id string) {
				screen, err := d.c.Read(id, d.lines)
				now := time.Now()
				d.mu.Lock()
				defer d.mu.Unlock()
				delete(inflight, id)
				p := d.panes[id]
				if p == nil {
					p = &paneState{ChangedAt: now}
					d.panes[id] = p
				}
				p.FetchedAt = now
				if err != nil {
					p.Err = err.Error()
					return
				}
				p.Err = ""
				if screen != p.Screen {
					p.Screen = screen
					p.ChangedAt = now
				}
			}(id)
		}
		time.Sleep(1500 * time.Millisecond)
	}
}

// ---- status detection ----

// statusOf derives a coarse per-agent state from the registry record and
// the captured pane. The text patterns target Claude Code's TUI (what
// warren spawns) plus generic y/n prompts; anything unrecognized falls
// back to recently-changed / quiet.
func statusOf(a client.AgentInfo, p *paneState, now time.Time) string {
	if !a.Alive {
		return "dead"
	}
	if !a.Controllable {
		return "uncontrolled"
	}
	if p == nil || p.FetchedAt.IsZero() {
		return "pending"
	}
	if p.Err != "" {
		// The hub 409s reads of a registered-but-gone agent; that's the
		// agent's death, not a host fault — don't paint it unreachable
		// for the up-to-5s window until agentsLoop refreshes Alive.
		if strings.Contains(p.Err, "is gone") || strings.Contains(p.Err, "no such agent") {
			return "dead"
		}
		return "unreachable"
	}
	tail := strings.ToLower(lastLines(p.Screen, 25))
	if strings.Contains(tail, "esc to interrupt") {
		return "working"
	}
	for _, pat := range attentionPatterns {
		if strings.Contains(tail, pat) {
			return "attention"
		}
	}
	// Claude's live spinner line, e.g. "✻ Orbiting… (8m 17s · ↓ 31.7k
	// tokens)". Needed because at narrow widths the status bar truncates
	// "esc to interrupt" away while the ⏵⏵ idle banner still shows.
	if spinnerRe.MatchString(tail) {
		return "working"
	}
	for _, pat := range idlePatterns {
		if strings.Contains(tail, pat) {
			return "idle"
		}
	}
	if now.Sub(p.ChangedAt) < 15*time.Second {
		return "active"
	}
	return "quiet"
}

var attentionPatterns = []string{
	"do you want",             // permission dialogs: "Do you want to proceed?" / "...make this edit?"
	"would you like",          // misc confirm dialogs
	"do you trust the files",  // folder-trust prompt fresh spawns sit at
	"bypass permissions mode", // the WARNING dialog (idle banner says "bypass permissions on")
	"not logged in",           // claude auth expired/missing
	"(y/n)",                   // generic shell prompts
	"❯ 1.",                    // numbered dialog with the selection arrow
}

// A running spinner verb with an elapsed-time paren: "orbiting… (8m" /
// "unfurling… (1m 49s". Completed ones ("thought for 50s") don't match.
var spinnerRe = regexp.MustCompile(`\p{L}+… \(\d`)

var idlePatterns = []string{
	"? for shortcuts",
	"⏵⏵",
	"shift+tab to cycle",
}

// lastLines returns the last n lines of s with trailing blank lines
// stripped first (tmux pads captures to the pane height).
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, " \t\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ---- HTTP ----

type dashAgent struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Host         string `json:"host"`
	Alive        bool   `json:"alive"`
	Controllable bool   `json:"controllable"`
	Registered   int64  `json:"registered"`
	Status       string `json:"status"`
	Screen       string `json:"screen"`
	ChangedS     int64  `json:"changed_s"` // seconds since the screen last changed; -1 unknown
	Err          string `json:"err,omitempty"`
}

func (d *dash) handler(addr string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page := strings.ReplaceAll(dashHTML, "{{TOKEN}}", d.token)
		page = strings.ReplaceAll(page, "{{NET}}", d.c.Net)
		w.Write([]byte(page))
	})
	mux.HandleFunc("GET /api/state", d.api(d.hState))
	mux.HandleFunc("GET /api/read", d.api(d.hReadOnce))
	mux.HandleFunc("POST /api/keys", d.api(d.hKeys))
	mux.HandleFunc("POST /api/kill", d.api(d.hKill))
	return d.hostCheck(addr, mux)
}

// hostCheck rejects requests whose Host header isn't the bound address —
// a DNS-rebinding page resolves its own name to 127.0.0.1 but still
// sends its own Host.
func (d *dash) hostCheck(addr string, next http.Handler) http.Handler {
	bindHost, bindPort, _ := net.SplitHostPort(addr)
	ok := map[string]bool{addr: true}
	if bindHost == "127.0.0.1" || bindHost == "::1" || bindHost == "localhost" {
		for _, h := range []string{"127.0.0.1", "[::1]", "localhost"} {
			ok[h+":"+bindPort] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok[r.Host] {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// api gates a handler behind the per-run token.
func (d *dash) api(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Dash-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(d.token)) != 1 {
			http.Error(w, `{"error":"bad dash token"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fn(w, r)
	}
}

func (d *dash) hState(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	d.mu.Lock()
	out := struct {
		Net         string            `json:"net"`
		Agents      []dashAgent       `json:"agents"`
		Unreachable map[string]string `json:"unreachable"`
		AgentsErr   string            `json:"agents_err,omitempty"`
	}{Net: d.c.Net, Unreachable: d.unreachable, AgentsErr: d.agentsErr}
	for _, a := range d.agents {
		name, host, err := proto.SplitAgent(a.Agent)
		if err != nil {
			name, host = a.Agent, "?"
		}
		p := d.panes[a.Agent]
		da := dashAgent{
			ID: a.Agent, Name: name, Host: host,
			Alive: a.Alive, Controllable: a.Controllable, Registered: a.Registered,
			Status: statusOf(a, p, now), ChangedS: -1,
		}
		if p != nil {
			da.Screen, da.Err = p.Screen, p.Err
			if !p.ChangedAt.IsZero() {
				da.ChangedS = int64(now.Sub(p.ChangedAt).Seconds())
			}
		}
		out.Agents = append(out.Agents, da)
	}
	d.mu.Unlock()
	json.NewEncoder(w).Encode(out)
}

func (d *dash) hReadOnce(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if lines <= 0 || lines > 5000 {
		lines = 300
	}
	screen, err := d.c.Read(agent, lines)
	if err != nil {
		apiErr(w, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"screen": screen})
}

func (d *dash) hKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Agent string `json:"agent"`
		Text  string `json:"text"`
		Enter bool   `json:"enter"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiErr(w, err)
		return
	}
	if err := d.c.Keys(req.Agent, req.Text, req.Enter); err != nil {
		apiErr(w, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (d *dash) hKill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiErr(w, err)
		return
	}
	killed, err := d.c.Kill(req.Agent)
	if err != nil {
		apiErr(w, err)
		return
	}
	// Reflect the kill immediately instead of waiting out the 5s
	// agents-refresh window.
	d.mu.Lock()
	for i := range d.agents {
		if d.agents[i].Agent == req.Agent {
			d.agents[i].Alive = false
		}
	}
	d.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]bool{"killed": killed})
}

func apiErr(w http.ResponseWriter, err error) {
	w.WriteHeader(http.StatusBadGateway)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
