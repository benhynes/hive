package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// protocolVersions are the MCP revisions this server speaks, newest first.
// The handshake echoes back the client's version when we know it, else our
// newest — which is what the spec asks a server to do on a version it does
// not recognize.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Tool is one callable exposed to the agent. Schema is the raw JSON Schema
// for the arguments object. Control marks a tool that needs the CONTROL
// credential — those are hidden entirely from an MSG-only agent rather than
// listed and then refused, so a model never plans around a tool it cannot use.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Control     bool
	Fn          func(ctx context.Context, args json.RawMessage) (string, error)
}

// Server maps JSON-RPC requests to tool calls. It is transport-agnostic:
// Handle is the whole protocol, and framing lives in ServeStdio (or, later,
// in an HTTP handler on the hub).
type Server struct {
	name    string
	version string
	tools   []Tool

	mu          sync.Mutex
	initialized bool
}

// NewServer builds a server exposing tools. Tools are listed in the order
// given.
func NewServer(name, version string, tools []Tool) *Server {
	return &Server{name: name, version: version, tools: tools}
}

// Handle processes one request and returns the response, or nil for a
// notification (which the spec forbids answering).
func (s *Server) Handle(ctx context.Context, req *Request) *Response {
	if req.JSONRPC != jsonrpcVersion {
		if req.isNotification() {
			return nil
		}
		return fail(req.ID, errInvalidRequest, "jsonrpc must be "+jsonrpcVersion)
	}

	switch req.Method {
	case "initialize":
		return s.initialize(req)
	case "notifications/initialized":
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
		return nil
	case "ping":
		return result(req.ID, struct{}{})
	case "tools/list":
		return s.listTools(req)
	case "tools/call":
		return s.callTool(ctx, req)
	}

	if req.isNotification() {
		return nil // unknown notifications are ignored, not errors
	}
	return fail(req.ID, errMethodNotFound, "unknown method "+req.Method)
}

func (s *Server) initialize(req *Request) *Response {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &p)
	}
	version := protocolVersions[0]
	for _, v := range protocolVersions {
		if v == p.ProtocolVersion {
			version = v
			break
		}
	}
	return result(req.ID, map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": s.name, "version": s.version},
	})
}

func (s *Server) listTools(req *Request) *Response {
	list := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		list = append(list, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.Schema,
		})
	}
	return result(req.ID, map[string]any{"tools": list})
}

func (s *Server) callTool(ctx context.Context, req *Request) *Response {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return fail(req.ID, errInvalidParams, "bad params: "+err.Error())
	}
	for _, t := range s.tools {
		if t.Name != p.Name {
			continue
		}
		out, err := t.Fn(ctx, p.Arguments)
		if err != nil {
			// A tool that fails is a result, not a protocol error: the model
			// must be able to read "undeliverable: no such agent" and adapt.
			return result(req.ID, toolResult(err.Error(), true))
		}
		return result(req.ID, toolResult(out, false))
	}
	return fail(req.ID, errInvalidParams, "unknown tool "+p.Name)
}

func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// ServeStdio runs the newline-delimited JSON framing loop until r is
// exhausted. Responses are written to w. Only Handle is protocol; this is
// only framing.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	// Envelopes carry message bodies (8 KiB cap) plus schema overhead; a
	// 1 MiB line ceiling clears that with room to spare.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	enc := json.NewEncoder(w)
	var mu sync.Mutex // one writer, so concurrent responses can't interleave
	write := func(resp *Response) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(resp)
	}

	// Each request runs in its own goroutine. hive_ask blocks until the peer
	// answers (60s by default), and a sequential loop would stall the whole
	// session behind it — including the client's keepalive pings. JSON-RPC
	// ids let responses come back out of order, which is exactly what this
	// needs.
	var wg sync.WaitGroup
	defer wg.Wait()

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			write(fail(nil, errParse, "parse error: "+err.Error()))
			continue
		}
		wg.Add(1)
		go func(req Request) {
			defer wg.Done()
			if resp := s.Handle(ctx, &req); resp != nil {
				write(resp)
			}
		}(req)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}
