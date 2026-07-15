package mcp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoTools is a minimal tool set: one that returns its argument, and one
// that blocks until released.
func echoTools(gate chan struct{}) []Tool {
	return []Tool{
		{
			Name:        "echo",
			Description: "echo",
			Schema:      schema(`{"type":"object","properties":{"s":{"type":"string"}}}`),
			Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					S string `json:"s"`
				}
				if err := decode(args, &a); err != nil {
					return "", err
				}
				return a.S, nil
			},
		},
		{
			Name:        "block",
			Description: "block until released",
			Schema:      schema(`{"type":"object"}`),
			Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
				<-gate
				return "released", nil
			},
		},
	}
}

func decodeResp(t *testing.T, line string) *struct {
	ID     json.RawMessage `json:"id"`
	Result map[string]any  `json:"result"`
	Error  *rpcError       `json:"error"`
} {
	t.Helper()
	var r struct {
		ID     json.RawMessage `json:"id"`
		Result map[string]any  `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("bad response %q: %v", line, err)
	}
	return &r
}

func TestHandshakeNegotiatesVersion(t *testing.T) {
	s := NewServer("hive", "1", nil)
	ctx := context.Background()

	// A version we speak is echoed back.
	resp := s.Handle(ctx, &Request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":"2024-11-05"}`),
	})
	got := resp.Result.(map[string]any)
	if got["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v, want the client's version echoed back", got["protocolVersion"])
	}

	// A version we do not know falls back to our newest, per spec.
	resp = s.Handle(ctx, &Request{
		JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":"1999-01-01"}`),
	})
	got = resp.Result.(map[string]any)
	if got["protocolVersion"] != protocolVersions[0] {
		t.Errorf("protocolVersion = %v, want fallback to %s", got["protocolVersion"], protocolVersions[0])
	}
}

func TestNotificationDrawsNoResponse(t *testing.T) {
	s := NewServer("hive", "1", nil)
	// A notification has no id. Answering one is a protocol violation.
	if resp := s.Handle(context.Background(), &Request{JSONRPC: "2.0", Method: "notifications/initialized"}); resp != nil {
		t.Fatalf("notification drew a response: %+v", resp)
	}
	if resp := s.Handle(context.Background(), &Request{JSONRPC: "2.0", Method: "notifications/unheard-of"}); resp != nil {
		t.Fatalf("unknown notification drew a response: %+v", resp)
	}
}

func TestUnknownMethodAndTool(t *testing.T) {
	s := NewServer("hive", "1", echoTools(nil))
	ctx := context.Background()

	resp := s.Handle(ctx, &Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "resources/list"})
	if resp.Error == nil || resp.Error.Code != errMethodNotFound {
		t.Errorf("unknown method: got %+v, want method-not-found", resp.Error)
	}

	resp = s.Handle(ctx, &Request{
		JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "tools/call",
		Params: json.RawMessage(`{"name":"nope","arguments":{}}`),
	})
	if resp.Error == nil || resp.Error.Code != errInvalidParams {
		t.Errorf("unknown tool: got %+v, want invalid-params", resp.Error)
	}
}

// A failing tool must come back as an isError *result*, not a JSON-RPC error:
// the model has to be able to read the failure and adapt to it.
func TestToolFailureIsAResultNotAProtocolError(t *testing.T) {
	s := NewServer("hive", "1", []Tool{{
		Name: "boom", Schema: schema(`{"type":"object"}`),
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			return "", io.ErrUnexpectedEOF
		},
	}})
	resp := s.Handle(context.Background(), &Request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: json.RawMessage(`{"name":"boom","arguments":{}}`),
	})
	if resp.Error != nil {
		t.Fatalf("tool failure became a protocol error: %+v", resp.Error)
	}
	res := resp.Result.(map[string]any)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("tool failure not flagged isError: %+v", res)
	}
	content := res["content"].([]map[string]any)
	if !strings.Contains(content[0]["text"].(string), "unexpected EOF") {
		t.Fatalf("error text lost: %+v", content)
	}
}

func TestServeStdioParseError(t *testing.T) {
	var out strings.Builder
	err := NewServer("hive", "1", nil).ServeStdio(context.Background(),
		strings.NewReader("{not json\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	r := decodeResp(t, out.String())
	if r.Error == nil || r.Error.Code != errParse {
		t.Fatalf("want parse error, got %s", out.String())
	}
}

// A blocking tool must not stall the rest of the session. hive_ask blocks for
// up to its timeout, and a sequential loop would hold up every other call —
// including the client's keepalive pings — behind it.
func TestBlockingToolDoesNotStallTheSession(t *testing.T) {
	gate := make(chan struct{})
	s := NewServer("hive", "1", echoTools(gate))

	pr, pw := io.Pipe()
	var mu sync.Mutex
	var out strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeStdio(context.Background(), pr, writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return out.WriteString(string(p))
		}))
	}()

	// Call the blocking tool, then a ping behind it.
	io.WriteString(pw, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"block","arguments":{}}}`+"\n")
	io.WriteString(pw, `{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n")

	// The ping must come back while id=1 is still blocked.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		got := out.String()
		mu.Unlock()
		if strings.Contains(got, `"id":2`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ping never answered while a blocking tool was in flight — the session serializes")
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(gate) // release the blocked tool
	pw.Close()
	<-done

	mu.Lock()
	got := out.String()
	mu.Unlock()
	if !strings.Contains(got, "released") {
		t.Fatalf("blocked tool never completed: %s", got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
