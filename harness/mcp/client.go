package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// ErrClosed is returned by Call once the transport is gone (Close was
// called, or the server process died and the reader goroutine hit EOF).
var ErrClosed = errors.New("mcp client: connection closed")

// Client talks JSON-RPC 2.0 to an MCP server over an arbitrary
// io.ReadWriteCloser (the stdio pipes of a child process in production, a
// net.Pipe in tests). The transport is injectable so the protocol layer
// can be tested in-process.
//
// Concurrency model: a single reader goroutine (readLoop) owns all
// incoming messages and routes responses to the pending-call channel
// registered for their id; writes are serialized by writeMu so the
// transport never sees interleaved partial lines. Call registers a
// per-id buffered channel before it writes, so a response can never be
// lost to a registration race.
type Client struct {
	rwc io.ReadWriteCloser

	// writeMu serializes everything written to rwc (requests,
	// notifications, and replies to server->client requests).
	writeMu sync.Mutex
	enc     *json.Encoder

	// mu guards nextID and the pending map (kept separate from
	// writeMu so a blocked write can never stall a response dispatch).
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *rpcResponse

	// readDone is closed when the reader goroutine exits for any
	// reason (EOF, transport error, or Close). Pending calls select on
	// it, so a dead server fails them fast instead of hanging.
	readDone chan struct{}
	readOnce sync.Once

	closeOnce sync.Once

	// serverProtocolVersion is whatever the server returned from
	// initialize, taken at face value (we do not assert equality with
	// the version we requested).
	serverProtocolVersion string
}

// NewClient performs the MCP handshake over rwc: an initialize request,
// then the notifications/initialized notification. On success the
// reader goroutine is running and the client is ready for Call.
//
// clientName is reported in clientInfo.name.
func NewClient(rwc io.ReadWriteCloser, clientName string) (*Client, error) {
	c := &Client{
		rwc:      rwc,
		enc:      json.NewEncoder(rwc),
		pending:  make(map[int64]chan *rpcResponse),
		readDone: make(chan struct{}),
	}
	go c.readLoop()

	ctx := context.Background()
	res, err := c.Call(ctx, "initialize", initializeParams{
		ProtocolVersion: protocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: clientName, Version: clientVersion},
	})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp initialize: %w", err)
	}
	var initRes initializeResult
	if err := json.Unmarshal(res, &initRes); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp initialize response: %w", err)
	}
	c.serverProtocolVersion = initRes.ProtocolVersion

	if err := c.notify("notifications/initialized", nil); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp initialized notification: %w", err)
	}
	return c, nil
}

// ServerProtocolVersion returns the protocolVersion the server declared
// during the handshake (informational; no compatibility assertion).
func (c *Client) ServerProtocolVersion() string { return c.serverProtocolVersion }

// Call sends a JSON-RPC request and waits for the matching response,
// honoring ctx cancellation. A ctx timeout/cancel unregisters the call
// and returns ctx.Err(); if the transport dies first, ErrClosed is
// returned promptly to every pending call.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	select {
	case <-c.readDone:
		c.mu.Unlock()
		return nil, ErrClosed
	default:
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *rpcResponse, 1) // buffered: the reader never blocks
	c.pending[id] = ch
	c.mu.Unlock()

	c.writeMu.Lock()
	err := c.enc.Encode(request{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	c.writeMu.Unlock()
	if err != nil {
		c.forget(id)
		// If the reader already saw EOF, the transport died: report
		// the same ErrClosed pending calls see, not a raw pipe error.
		if c.closed() {
			return nil, ErrClosed
		}
		return nil, fmt.Errorf("mcp write %s: %w", method, err)
	}

	select {
	case resp := <-ch:
		return unwrap(resp)
	case <-ctx.Done():
		c.forget(id)
		return nil, ctx.Err()
	case <-c.readDone:
		// The transport died; a response that raced in just before
		// still wins so we don't fail a completed call.
		select {
		case resp := <-ch:
			return unwrap(resp)
		default:
		}
		c.forget(id)
		return nil, ErrClosed
	}
}

func unwrap(resp *rpcResponse) (json.RawMessage, error) {
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

func (c *Client) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// closed reports whether the reader goroutine has exited (transport
// dead or Close called).
func (c *Client) closed() bool {
	select {
	case <-c.readDone:
		return true
	default:
		return false
	}
}

func (c *Client) notify(method string, params any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.enc.Encode(notification{JSONRPC: "2.0", Method: method, Params: params})
}

// Close shuts the transport down (for a spawned server this also kills
// the child process: StartServer's transport Close does the Kill). Safe
// to call more than once; only the first call's close is attempted.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.rwc.Close()
	})
	return err
}

// readLoop is the single reader: one JSON object per line. Blank lines
// are ignored, malformed lines are logged and skipped (a chatty server
// must not kill the client), messages without an id are notifications
// (ignored), messages with both id and method are server->client
// requests (answered minimally), and everything else with an id is a
// response routed to its pending call.
func (c *Client) readLoop() {
	defer c.readOnce.Do(func() { close(c.readDone) })

	sc := bufio.NewScanner(c.rwc)
	// MCP tool results can be much larger than the 64 KB default.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var msg inbound
		if err := json.Unmarshal(line, &msg); err != nil {
			fmt.Fprintf(os.Stderr, "mcp: skipping malformed message: %v\n", err)
			continue
		}
		switch {
		case len(msg.ID) > 0 && msg.Method != "":
			c.answerServerRequest(msg.ID, msg.Method)
		case len(msg.ID) > 0:
			c.deliver(msg)
		default:
			// Server notification: nothing to do.
		}
	}
	// Transport dead (EOF or write/read error): the deferred close of
	// readDone fails all pending calls.
}

// deliver routes a response to the pending call registered for its id.
// Our ids are integers, so a non-integer id can only come from a
// confused server and is logged rather than matched.
func (c *Client) deliver(msg inbound) {
	var id int64
	if err := json.Unmarshal(msg.ID, &id); err != nil {
		fmt.Fprintf(os.Stderr, "mcp: response with non-integer id %s, ignoring\n", msg.ID)
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !ok {
		fmt.Fprintf(os.Stderr, "mcp: response for unknown id %d, ignoring\n", id)
		return
	}
	ch <- &rpcResponse{Result: msg.Result, Error: msg.Error}
}

// answerServerRequest replies to a server->client request. We support
// nothing except ping (answered with an empty result, per spec); every
// other method gets a JSON-RPC -32601 method-not-found error.
func (c *Client) answerServerRequest(id json.RawMessage, method string) {
	resp := rawResponse{JSONRPC: "2.0", ID: id}
	if method == "ping" {
		resp.Result = json.RawMessage("{}")
	} else {
		resp.Error = &errorObject{Code: methodNotFound, Message: "method not found: " + method}
	}
	c.writeMu.Lock()
	err := c.enc.Encode(resp)
	c.writeMu.Unlock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp: failed to answer server request %q: %v\n", method, err)
	}
}
