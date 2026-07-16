package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/benhynes/hive/internal/client"
	"github.com/benhynes/hive/internal/mcp"
	"github.com/benhynes/hive/internal/proto"
)

// mcpServerVersion is reported in the MCP handshake. It tracks the hive wire
// protocol version, which is what actually constrains the tools.
const mcpServerVersion = "1"

// runMCP serves the mesh over MCP on stdin/stdout, so an agent calls
// hive_send / hive_ask / hive_recv as native tools instead of shelling out.
//
// A spawned or `hive run` agent uses the identity already injected in its
// environment. A normally-launched agent with Hive configured as an MCP server
// auto-registers a short-lived identity instead, so joining the message mesh
// does not depend on owning or adopting a terminal pane:
//
//	hive mcp --name alice       # --name is optional
//
// stdout is the protocol channel — nothing may print there but JSON-RPC.
// Diagnostics go to stderr.
func runMCP(args []string) error {
	fs := flags("mcp", args)
	list := fs.Bool("list", false, "print the tools this agent would be offered, then exit")
	name := fs.String("name", "", "agent name for automatic registration (default: generated)")
	logFile := fs.String("log-file", "", "append diagnostics to this file (stderr remains the protocol-safe default)")
	fs.Parse2()
	var diagnostic io.Writer = os.Stderr
	var log *os.File
	if *logFile != "" {
		var err error
		log, err = os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("open MCP log: %w", err)
		}
		defer log.Close()
		diagnostic = io.MultiWriter(os.Stderr, log)
		fmt.Fprintf(log, "hive mcp: starting at %s\n", time.Now().UTC().Format(time.RFC3339Nano))
	}

	c, err := resolveMCPClient(*fs.net)
	if err != nil {
		fmt.Fprintf(diagnostic, "hive mcp: resolve client: %v\n", err)
		return err
	}

	if *list {
		if err := validateMCPName(c, *name); err != nil {
			return err
		}
		tools := mcp.Tools(c)
		layer := "MSG"
		if c.HasControl() {
			layer = "MSG + CONTROL"
		}
		agent := c.Agent
		if agent == "" {
			agent = "(generated on start)"
			if *name != "" {
				agent = *name + "@(auto)"
			}
		}
		fmt.Printf("net %s · agent %s · layer %s\n", c.Net, agent, layer)
		fmt.Println(strings.Join(mcp.Names(tools), "\n"))
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	signalDone := make(chan struct{})
	go func() {
		defer close(signalDone)
		select {
		case <-sigc:
			cancel()
			// Scanner.Scan cannot observe context cancellation on its own.
			// Closing our read side is safe during process shutdown and makes
			// SIGINT/SIGTERM a real stdio lifetime boundary.
			_ = os.Stdin.Close()
		case <-ctx.Done():
		}
	}()
	defer func() {
		signal.Stop(sigc)
		cancel()
		<-signalDone
	}()
	c.SetContext(ctx)

	if err := validateMCPName(c, *name); err != nil {
		return err
	}
	needsEnrollment := c.Agent == ""
	gate := newMCPIdentityGate(c, *name)
	tools := mcp.Tools(c)
	if needsEnrollment {
		tools = gateToolsOnIdentity(tools, gate)
	}
	var leaseWG sync.WaitGroup
	if needsEnrollment {
		leaseWG.Add(1)
		go func() {
			defer leaseWG.Done()
			gate.enrollAndMaintain(ctx)
		}()
	}

	srv := mcp.NewServer("hive", mcpServerVersion, tools)
	err = srv.ServeStdio(ctx, cancelOnEOFReader{Reader: os.Stdin, cancel: cancel}, os.Stdout)
	if err != nil {
		fmt.Fprintf(diagnostic, "hive mcp: protocol exit: %v\n", err)
	} else {
		fmt.Fprintln(diagnostic, "hive mcp: protocol EOF")
	}
	cancel()
	leaseWG.Wait()
	if needsEnrollment && gate.enrolledIdentity() {
		// Cleanup must not inherit the now-cancelled session context or the
		// normal 35-second request timeout. Lease expiry is the crash fallback;
		// shutdown cleanup is intentionally brief and best-effort.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		c.SetContext(cleanupCtx)
		var cleanupErr error
		if *name == "" {
			cleanupErr = c.Deregister("")
		} else {
			_, cleanupErr = c.ReleaseLease()
		}
		cleanupCancel()
		if cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "hive mcp: release %s: %v (lease will expire)\n", c.Agent, cleanupErr)
		}
	}
	return err
}

// mcpIdentityGate lets the MCP protocol initialize even when the hub is still
// starting or a stable name's old lease has not expired. Enrollment retries in
// the background and every tool call gates on it, so no operation can fall
// through with the bootstrap network token and impersonate "human".
type mcpIdentityGate struct {
	mu             sync.RWMutex
	c              *client.Client
	requested      string
	bootstrapToken string
	enrolled       bool
}

func newMCPIdentityGate(c *client.Client, requested string) *mcpIdentityGate {
	return &mcpIdentityGate{c: c, requested: requested, bootstrapToken: c.Token}
}

func (g *mcpIdentityGate) ensure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.enrolled || g.c.Agent != "" {
		g.enrolled = true
		return nil
	}
	_, err := ensureMCPIdentity(g.c, g.requested)
	if err == nil {
		g.enrolled = true
	}
	return err
}

func (g *mcpIdentityGate) enrolledIdentity() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.enrolled
}

// resetIdentity returns an automatically enrolled client to its network
// bootstrap credential after the hub definitively rejects the minted token.
// expected prevents two concurrent failed requests from clearing a newer
// enrollment. Callers must not hold g.mu.
func (g *mcpIdentityGate) resetIdentity(expected string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.enrolled || g.c.Token != expected {
		return false
	}
	g.c.Agent = ""
	g.c.Token = g.bootstrapToken
	g.enrolled = false
	return true
}

func (g *mcpIdentityGate) enrollAndMaintain(ctx context.Context) {
	failed := false
	for {
		if err := g.ensure(ctx); err == nil {
			failed = false
			if maintainMCPLease(ctx, g) {
				// The daemon definitively rejected this identity (for example,
				// its disposable record was pruned after an extended outage).
				// Re-enter enrollment instead of leaving this MCP session dead.
				continue
			}
			return
		} else if ctx.Err() == nil && !failed {
			fmt.Fprintf(os.Stderr, "hive mcp: enrollment deferred; will retry: %v\n", err)
			failed = true
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func gateToolsOnIdentity(tools []mcp.Tool, gate *mcpIdentityGate) []mcp.Tool {
	for i := range tools {
		call := tools[i].Fn
		usesIdentityToken := isIdentityAuthenticatedTool(tools[i].Name)
		tools[i].Fn = func(ctx context.Context, args json.RawMessage) (string, error) {
			for {
				if err := gate.ensure(ctx); err != nil {
					return "", fmt.Errorf("hive identity is not enrolled yet: %w", err)
				}
				// Hold a shared identity lock for the request. Tools remain
				// concurrent with one another and with heartbeats, while a lost
				// identity cannot be rewritten underneath an in-flight request.
				gate.mu.RLock()
				if !gate.enrolled {
					gate.mu.RUnlock()
					continue
				}
				token := gate.c.Token
				out, err := call(ctx, args)
				gate.mu.RUnlock()
				if usesIdentityToken && client.IsHTTPStatus(err, 401) {
					gate.resetIdentity(token)
					continue
				}
				return out, err
			}
		}
	}
	return tools
}

func isIdentityAuthenticatedTool(name string) bool {
	switch name {
	case "hive_agents", "hive_send", "hive_recv", "hive_ask", "hive_asks", "hive_answer":
		return true
	default:
		// Control tools authenticate with c.Control. Re-enrolling the MSG
		// identity cannot repair an invalid host-scoped control credential.
		return false
	}
}

// resolveMCPClient accepts either an injected identity or a network bootstrap
// credential. The latter includes environment-only deployments that provide
// HIVE_ADDR/HIVE_NET/HIVE_TOKEN without mounting Hive's local config.
func resolveMCPClient(netFlag string) (*client.Client, error) {
	if os.Getenv("HIVE_AGENT") != "" {
		return client.ResolveAgent(netFlag)
	}
	c, err := client.ResolveBootstrap(netFlag)
	if err != nil {
		return nil, err
	}
	// ResolveBootstrap deliberately strips CONTROL for child launchers. An MCP
	// sidecar may expose control tools, but only when that capability was
	// explicitly injected into this process; never inherit it from net.json.
	if tok := os.Getenv("HIVE_CONTROL_TOKEN"); tok != "" {
		c.Control = tok
		c.ControlHost = os.Getenv("HIVE_CONTROL_HOST")
	}
	return c, nil
}

type cancelOnEOFReader struct {
	io.Reader
	cancel context.CancelFunc
}

func (r cancelOnEOFReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil {
		r.cancel()
	}
	return n, err
}

// ensureMCPIdentity preserves identities injected by hive spawn/run and
// automatically leases one for a normally-launched MCP client. The bootstrap
// network credential is replaced with the minted per-agent credential before
// a gated tool is allowed to run, so every message has a server-stamped agent
// identity.
func ensureMCPIdentity(c *client.Client, requested string) (bool, error) {
	if err := validateMCPName(c, requested); err != nil {
		return false, err
	}
	if c.Agent != "" {
		return false, nil
	}
	generated := requested == ""
	var resp client.RegisterResp
	var err error
	if generated {
		// Generated inbox names are also their durable storage keys. Use 64 bits
		// of entropy and retry the already-vanishing collision case so a process
		// can never inherit another generated process's mailbox by name.
		for attempt := 0; attempt < 4; attempt++ {
			requested = "agent-" + proto.NewToken()[:16]
			resp, err = c.RegisterEphemeralLease(requested, client.DefaultLeaseSeconds)
			if err == nil || !strings.Contains(err.Error(), "taken by a live agent") {
				break
			}
		}
	} else {
		resp, err = c.RegisterLease(requested, "", 0, client.DefaultLeaseSeconds)
	}
	if err != nil {
		return false, fmt.Errorf("auto-register %q: %w", requested, err)
	}
	c.Agent, c.Token = resp.Agent, resp.Token
	fmt.Fprintf(os.Stderr, "hive mcp: auto-registered %s (leased)\n", c.Agent)
	return true, nil
}

func validateMCPName(c *client.Client, requested string) error {
	// An identity injected by hive spawn/run is authoritative. This lets a
	// runtime keep one global `hive mcp --name ...` configuration while a
	// launcher supplies a distinct per-process identity; the configured name is
	// not interpreted at all in that mode.
	if c.Agent != "" {
		return nil
	}
	if requested != "" && !proto.ValidName(requested) {
		return fmt.Errorf("bad agent name %q (want [a-z0-9][a-z0-9_-]*, <=32)", requested)
	}
	return nil
}

// maintainMCPLease ties presence to the stdio MCP subprocess. Discovery stops
// reporting this process alive if the client vanishes without clean cleanup;
// explicitly named mailboxes remain durable, while generated ones are retired.
// maintainMCPLease returns true only when the hub definitively rejected the
// current identity and the enrollment loop should mint/reclaim one. Transient
// transport failures keep retrying the same token so a suspended machine can
// recover its retained lease record and mailbox.
func maintainMCPLease(ctx context.Context, gate *mcpIdentityGate) bool {
	interval := time.Duration(client.DefaultHeartbeatSeconds) * time.Second
	retry := interval / 3
	timer := time.NewTimer(interval)
	defer timer.Stop()
	failed := false
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			beatCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			gate.mu.RLock()
			if !gate.enrolled {
				gate.mu.RUnlock()
				cancel()
				return true
			}
			token := gate.c.Token
			agent := gate.c.Agent
			_, err := gate.c.HeartbeatContext(beatCtx)
			gate.mu.RUnlock()
			cancel()
			if ctx.Err() != nil {
				return false
			}
			if client.IsHTTPStatus(err, 401, 404, 409) {
				if gate.resetIdentity(token) {
					fmt.Fprintln(os.Stderr, "hive mcp: identity expired or was replaced; re-enrolling")
				}
				return true
			}
			if err != nil {
				if !failed {
					fmt.Fprintf(os.Stderr, "hive mcp: heartbeat %s: %v\n", agent, err)
				}
				failed = true
				timer.Reset(retry)
			} else {
				failed = false
				timer.Reset(interval)
			}
		}
	}
}
