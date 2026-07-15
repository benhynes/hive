// Package mcp serves the hive mesh as a Model Context Protocol server, so
// an agent calls hive_send / hive_ask / hive_recv as native tools instead
// of shelling out to the CLI.
//
// The protocol layer here is deliberately transport-agnostic: Server.Handle
// maps one JSON-RPC request to one response and knows nothing about where
// the bytes came from. ServeStdio is a thin framing loop on top of it, and
// a future HTTP endpoint on the hub would be a second caller of Handle.
//
// Only the `tools` primitive is implemented — it is all a mesh needs, and
// implementing it directly keeps hive a zero-dependency binary.
package mcp

import "encoding/json"

// jsonrpcVersion is the only version this server speaks.
const jsonrpcVersion = "2.0"

// JSON-RPC 2.0 error codes (the negative-32xxx block is reserved by the spec).
const (
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
)

// Request is an inbound JSON-RPC request or notification. A notification is
// a request with no id, and per the spec it must not be answered.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the peer expects no response.
func (r *Request) isNotification() bool { return len(r.ID) == 0 }

// Response is an outbound JSON-RPC response. Exactly one of Result and Error
// is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func result(id json.RawMessage, v any) *Response {
	return &Response{JSONRPC: jsonrpcVersion, ID: id, Result: v}
}

func fail(id json.RawMessage, code int, msg string) *Response {
	return &Response{JSONRPC: jsonrpcVersion, ID: id, Error: &rpcError{Code: code, Message: msg}}
}
