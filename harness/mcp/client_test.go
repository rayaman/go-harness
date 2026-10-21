package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"ai-harness/harness"
)

// -----------------------------------------------------------------------------
// Shared minimal fake MCP server (newline-delimited JSON-RPC 2.0). Used
// in-process over net.Pipe by these tests, and by server_test.go's
// subprocess re-exec mode over the child's real stdio pipes.
// -----------------------------------------------------------------------------

// fakeOpts tweaks the fake's behavior per test.
type fakeOpts struct {
	// onMsg records every request/notification method the server read,
	// plus its decoded params, in arrival order.
	onMsg func(method string, params map[string]any)
	// serverRequests injects two server->client REQUESTS (ping and an
	// unsupported method) before answering tools/list, and validates
	// the client's replies to them.
	serverRequests bool
	// silent swallows tools/call: no response ever (ctx-cancellation test).
	silent bool
}

// fakeToolListJSON is the tools/list result the fake advertises: "echo"
// with an inputSchema, "boom" without one (exercises the default).
const fakeToolListJSON = `{"tools":[` +
	`{"name":"echo","description":"echoes things back","inputSchema":{"type":"object","properties":{"msg":{"type":"string"}}}},` +
	`{"name":"boom","description":"always fails"}` +
	`]}`

// serveFake reads newline-delimited JSON-RPC from r and replies on w.
// Returns when the input reaches EOF or w breaks.
func serveFake(r io.Reader, w io.Writer, opts fakeOpts) error {
	sc := bufio.NewScanner(r)
	enc := json.NewEncoder(w)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id,omitempty"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		var params map[string]any
		_ = json.Unmarshal(msg.Params, &params)
		if opts.onMsg != nil {
			opts.onMsg(msg.Method, params)
		}
		isRequest := len(msg.ID) > 0
		if !isRequest {
			continue // notification: no reply
		}

		switch msg.Method {
		case "initialize":
			if err := enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": msg.ID,
				"result": map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "fake-mcp", "version": "test"},
				},
			}); err != nil {
				return err
			}
		case "tools/list":
			if opts.serverRequests {
				if err := injectServerRequests(sc, enc); err != nil {
					return err
				}
			}
			if err := enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": msg.ID, "result": json.RawMessage(fakeToolListJSON),
			}); err != nil {
				return err
			}
		case "tools/call":
			if opts.silent {
				continue
			}
			name, _ := params["name"].(string)
			var result json.RawMessage
			switch name {
			case "echo":
				result = json.RawMessage(`{"content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}`)
			case "boom":
				result = json.RawMessage(`{"content":[{"type":"text","text":"kaboom"}],"isError":true}`)
			case "photo":
				result = json.RawMessage(`{"content":[{"type":"image","data":"ZmFrZQ==","mimeType":"image/png"}]}`)
			default:
				if err := enc.Encode(map[string]any{
					"jsonrpc": "2.0", "id": msg.ID,
					"error": map[string]any{"code": -32602, "message": "unknown tool " + name},
				}); err != nil {
					return err
				}
				continue
			}
			if err := enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": msg.ID, "result": result,
			}); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

// injectServerRequests sends one supported (ping) and one unsupported
// (shutdown) server->client REQUEST and validates the client's replies:
// ping must be answered with an empty result, shutdown with -32601.
func injectServerRequests(sc *bufio.Scanner, enc *json.Encoder) error {
	send := func(id, method string) error {
		return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method})
	}
	readReply := func() (map[string]any, error) {
		if !sc.Scan() {
			return nil, errors.New("client closed while answering server request")
		}
		var reply map[string]any
		return reply, json.Unmarshal(sc.Bytes(), &reply)
	}

	if err := send("s1", "ping"); err != nil {
		return err
	}
	reply, err := readReply()
	if err != nil {
		return err
	}
	if res, ok := reply["result"].(map[string]any); !ok || len(res) != 0 {
		return errors.New("ping reply missing empty result")
	}

	if err := send("s2", "shutdown"); err != nil {
		return err
	}
	reply, err = readReply()
	if err != nil {
		return err
	}
	e, _ := reply["error"].(map[string]any)
	if e == nil {
		return errors.New("shutdown reply missing error object")
	}
	if code, _ := e["code"].(float64); int(code) != methodNotFound {
		return errors.New("shutdown reply not -32601")
	}
	return nil
}

// newPipeClient wires a Client to an in-process fake server over
// net.Pipe and returns both ends' halves plus the recorded message log.
func newPipeClient(t *testing.T, opts fakeOpts) (*Client, net.Conn) {
	t.Helper()
	clientEnd, serverEnd := net.Pipe()
	go func() {
		_ = serveFake(serverEnd, serverEnd, opts)
	}()
	c, err := NewClient(clientEnd, "test-client")
	if err != nil {
		serverEnd.Close()
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { c.Close(); serverEnd.Close() })
	return c, serverEnd
}

// -----------------------------------------------------------------------------
// Protocol tests
// -----------------------------------------------------------------------------

// TestHandshakeOrder asserts the client sends initialize (with the spec
// params) and then the notifications/initialized notification, and that
// the server's protocolVersion is kept as-is.
func TestHandshakeOrder(t *testing.T) {
	var methods []string
	var initParams map[string]any
	c, _ := newPipeClient(t, fakeOpts{
		onMsg: func(m string, p map[string]any) {
			methods = append(methods, m)
			if m == "initialize" {
				initParams = p
			}
		},
	})

	ctx := context.Background()
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err) // barrier: fake read tools/list, so all earlier messages were recorded
	}
	want := []string{"initialize", "notifications/initialized", "tools/list"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("message order = %v, want %v", methods, want)
	}
	if pv, _ := initParams["protocolVersion"].(string); pv != "2025-06-18" {
		t.Errorf("initialize protocolVersion = %v, want 2025-06-18", initParams["protocolVersion"])
	}
	info, _ := initParams["clientInfo"].(map[string]any)
	if info == nil || info["name"] != "test-client" {
		t.Errorf("initialize clientInfo = %v, want name test-client", initParams["clientInfo"])
	}
	if c.ServerProtocolVersion() != "2025-06-18" {
		t.Errorf("ServerProtocolVersion = %q", c.ServerProtocolVersion())
	}
}

// TestListToolsDecode checks that two tools decode, including the one
// with a missing inputSchema, and that the adapter defaults its schema.
func TestListToolsDecode(t *testing.T) {
	c, _ := newPipeClient(t, fakeOpts{})
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	if tools[0].Name != "echo" || len(tools[0].InputSchema) == 0 {
		t.Errorf("echo tool = %+v, want inputSchema present", tools[0])
	}
	if tools[1].Name != "boom" || len(tools[1].InputSchema) != 0 {
		t.Errorf("boom tool = %+v, want missing inputSchema", tools[1])
	}

	ad := &Tool{c: c, info: tools[1]}
	params := ad.Parameters()
	if params["type"] != "object" {
		t.Errorf("default Parameters type = %v, want object", params["type"])
	}
	if props, ok := params["properties"].(map[string]any); !ok || len(props) != 0 {
		t.Errorf("default Parameters properties = %v, want empty object", params["properties"])
	}
	if got := (&Tool{c: c, info: tools[0]}).Parameters(); got["type"] != "object" {
		t.Errorf("echo Parameters = %v, want its schema", got)
	}
}

// TestCallToolFlatten checks two text parts joined by newline.
func TestCallToolFlatten(t *testing.T) {
	c, _ := newPipeClient(t, fakeOpts{})
	out, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != "first\nsecond" {
		t.Fatalf("flattened output = %q, want %q", out, "first\nsecond")
	}
}

// TestCallToolIsError checks the "error: " prefix on isError results.
func TestCallToolIsError(t *testing.T) {
	c, _ := newPipeClient(t, fakeOpts{})
	out, err := c.CallTool(context.Background(), "boom", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.HasPrefix(out, "error: ") || out != "error: kaboom" {
		t.Fatalf("isError output = %q, want %q", out, "error: kaboom")
	}
}

// TestCallToolNonText checks the placeholder line for image parts.
func TestCallToolNonText(t *testing.T) {
	c, _ := newPipeClient(t, fakeOpts{})
	out, err := c.CallTool(context.Background(), "photo", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != "[non-text content: image]" {
		t.Fatalf("non-text output = %q, want [non-text content: image]", out)
	}
}

// TestServerRequestPing checks a server->client ping is answered with
// an empty result (and an unsupported method with -32601, validated
// inside the fake) while the client's own tools/list still completes.
func TestServerRequestPing(t *testing.T) {
	c, _ := newPipeClient(t, fakeOpts{serverRequests: true})
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools after server requests: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools after server requests, want 2", len(tools))
	}
}

// TestCloseIdempotent checks Close twice does not panic or error twice.
func TestCloseIdempotent(t *testing.T) {
	clientEnd, serverEnd := net.Pipe()
	go func() { _ = serveFake(serverEnd, serverEnd, fakeOpts{}) }()
	c, err := NewClient(clientEnd, "test-client")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	serverEnd.Close()
}

// TestCallTransportDeath: when the transport dies, Call must fail fast
// with ErrClosed rather than hang.
func TestCallTransportDeath(t *testing.T) {
	clientEnd, serverEnd := net.Pipe()
	go func() { _ = serveFake(serverEnd, serverEnd, fakeOpts{}) }()
	c, err := NewClient(clientEnd, "test-client")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// Kill the server side: the reader goroutine sees EOF.
	serverEnd.Close()
	// Wait until the reader goroutine has actually observed the death,
	// so the test asserts the fast-fail path rather than racing it.
	for i := 0; i < 100 && !c.closed(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !c.closed() {
		t.Fatal("reader goroutine never observed transport death")
	}

	start := time.Now()
	_, err = c.Call(context.Background(), "tools/list", map[string]any{})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Call after transport death: err = %v, want ErrClosed", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Call took %v after death, want prompt failure", elapsed)
	}
	c.Close()
}

// TestCallContextCancel: a server that never answers tools/call must
// not block the caller; a 200ms context returns an error promptly.
func TestCallContextCancel(t *testing.T) {
	c, _ := newPipeClient(t, fakeOpts{silent: true})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Call(ctx, "tools/call", map[string]any{"name": "echo"})
	if err == nil {
		t.Fatal("Call with expiring ctx returned nil error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Call took %v, want prompt ctx error", elapsed)
	}
}

// -----------------------------------------------------------------------------
// Registry adapter
// -----------------------------------------------------------------------------

// stubTool is a trivial harness.Tool used to create a name collision.
type stubTool struct{ name string }

func (s stubTool) Name() string        { return s.name }
func (s stubTool) Description() string { return "stub" }
func (s stubTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (s stubTool) Execute(json.RawMessage) (string, error) { return "stub", nil }

// TestLoadToolsRegisters checks discovery + registration, the
// collision rename (<server>_<name>), the returned count, and that the
// adapter opts out of parallel execution.
func TestLoadToolsRegisters(t *testing.T) {
	c, _ := newPipeClient(t, fakeOpts{})

	registry := harness.NewRegistry()
	registry.Register(stubTool{name: "echo"}) // collide with the fake's "echo"

	n, err := LoadTools(context.Background(), c, registry, "srv")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	if n != 2 {
		t.Fatalf("LoadTools count = %d, want 2", n)
	}
	if _, ok := registry.Get("boom"); !ok {
		t.Error("tool boom not registered")
	}
	renamed, ok := registry.Get("srv_echo")
	if !ok {
		t.Fatal("colliding tool not registered as srv_echo")
	}
	// The original must not be clobbered.
	if kept, ok := registry.Get("echo"); !ok || kept.Name() != "echo" || kept.Description() != "stub" {
		t.Errorf("pre-existing echo was clobbered: %v", kept)
	}
	if renamed.Description() != "echoes things back" {
		t.Errorf("srv_echo description = %q", renamed.Description())
	}

	// MCP tools must be Serial: they talk to a single foreign process.
	st, ok := renamed.(harness.SerialTyper)
	if !ok {
		t.Fatal("MCP tool does not implement harness.SerialTyper")
	}
	if !st.Serial() {
		t.Error("MCP tool Serial() = false, want true")
	}
}
