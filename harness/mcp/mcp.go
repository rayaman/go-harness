// Package mcp implements a minimal Model Context Protocol client over the
// stdio transport (roadmap 2.2): newline-delimited JSON-RPC 2.0, one JSON
// object per line in each direction (no Content-Length/LSP framing).
//
// The client speaks exactly three request methods (initialize, tools/list,
// tools/call) plus the notifications/initialized notification, and answers
// server->client requests minimally (ping -> {}, anything else -> -32601).
package mcp

import (
	"encoding/json"
	"strconv"
)

// protocolVersion is the MCP revision advertised in the initialize
// request. A server's returned version is taken at face value.
const protocolVersion = "2025-06-18"

// Standard JSON-RPC error codes used by this package.
const (
	methodNotFound = -32601
)

// clientVersion is reported in clientInfo (the harness is a minimal
// client, hence 0.1.0 regardless of the app's own version).
const clientVersion = "0.1.0"

// request is an outgoing JSON-RPC 2.0 request. IDs are monotonically
// increasing integers assigned by Client.Call.
type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// notification is an outgoing JSON-RPC 2.0 notification: same shape as a
// request but with no id, so no response is ever expected.
type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// errorObject is the JSON-RPC 2.0 error member of a response.
type errorObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *errorObject) Error() string {
	return "jsonrpc error " + strconv.Itoa(e.Code) + ": " + e.Message
}

// rpcResponse is the decoded response to one of our requests. Exactly one
// of Result/Error is meaningful.
type rpcResponse struct {
	Result json.RawMessage
	Error  *errorObject
}

// rawResponse is an outgoing response (used when the SERVER sends us a
// request and we must answer it): the id is echoed back verbatim as raw
// JSON because we cannot assume it is one of our own integer ids.
type rawResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *errorObject    `json:"error,omitempty"`
}

// inbound is the envelope for every message arriving from the server. A
// message with no id is a notification (ignored); a message with both id
// and method is a server->client REQUEST that must be answered; anything
// else with an id is a response to one of our calls.
type inbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *errorObject    `json:"error,omitempty"`
}

// initializeParams is the payload of the initialize request.
type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// initializeResult is what we bother to decode from the server's
// initialize response. The returned protocolVersion is taken at face
// value (no equality assertion against our requested version).
type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      *clientInfo    `json:"serverInfo,omitempty"`
}
