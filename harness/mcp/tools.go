package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"ai-harness/harness"
)

// ToolInfo is one entry of a tools/list response.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// contentPart is one item of a tools/call result's content array. Text
// parts carry text; image/resource parts carry other fields we do not
// interpret beyond their type tag.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// callToolResult is the decoded result of a tools/call request.
type callToolResult struct {
	Content []contentPart `json:"content"`
	IsError bool          `json:"isError"`
}

// ListTools issues tools/list and decodes the server's tool table.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	res, err := c.Call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("mcp tools/list: %w", err)
	}
	var payload struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(res, &payload); err != nil {
		return nil, fmt.Errorf("mcp tools/list response: %w", err)
	}
	return payload.Tools, nil
}

// CallTool issues tools/call and flattens the result into the single
// string fed back to the model: every type:"text" part joined by
// newlines, non-text parts replaced by a "[non-text content: <type>]"
// placeholder line. When the server flags isError, the flattened text is
// prefixed with "error: " so the model sees it as a failure.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	res, err := c.Call(ctx, "tools/call", struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{Name: name, Arguments: args})
	if err != nil {
		return "", fmt.Errorf("mcp tools/call %s: %w", name, err)
	}
	var payload callToolResult
	if err := json.Unmarshal(res, &payload); err != nil {
		return "", fmt.Errorf("mcp tools/call %s response: %w", name, err)
	}
	parts := make([]string, 0, len(payload.Content))
	for _, p := range payload.Content {
		if p.Type == "text" {
			parts = append(parts, p.Text)
		} else {
			parts = append(parts, fmt.Sprintf("[non-text content: %s]", p.Type))
		}
	}
	out := strings.Join(parts, "\n")
	if payload.IsError {
		out = "error: " + out
	}
	return out, nil
}

// Tool adapts one discovered MCP tool to the harness.Tool interface so
// it can sit in a harness.Registry next to the builtins and Lua tools.
type Tool struct {
	c    *Client
	info ToolInfo
}

// defaultInputSchema keeps the request body (and the model-facing
// schema) valid when the server omitted or malformed its inputSchema.
func defaultInputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *Tool) Name() string        { return t.info.Name }
func (t *Tool) Description() string { return t.info.Description }

// Parameters returns the server's inputSchema decoded into a generic
// map. A missing or invalid schema falls back to an empty object schema
// so the tool call still validates against a well-formed body.
func (t *Tool) Parameters() map[string]any {
	if len(t.info.InputSchema) == 0 {
		return defaultInputSchema()
	}
	var schema map[string]any
	if err := json.Unmarshal(t.info.InputSchema, &schema); err != nil || schema == nil {
		return defaultInputSchema()
	}
	return schema
}

// Execute forwards the model's raw JSON arguments to tools/call.
func (t *Tool) Execute(args json.RawMessage) (string, error) {
	return t.c.CallTool(context.Background(), t.info.Name, args)
}

// Serial reports true: an MCP server is a single foreign process whose
// concurrency safety we cannot know, so MCP tools never share the
// parallel pool (roadmap 2.1). Same rationale as the LuaTool marker: the
// mutex inside Client already prevents data races on the transport, but
// reporting Serial keeps the harness scheduler from queueing two calls
// of this tool concurrently against a process that may not be
// goroutine-safe.
func (t *Tool) Serial() bool { return true }

// LoadTools discovers the server's tools via tools/list and registers
// each one in r, prefixed "<serverName>_<name>" only when the plain
// name is already taken (collision note printed to stderr). Returns the
// number of tools registered.
func LoadTools(ctx context.Context, c *Client, r *harness.Registry, serverName string) (int, error) {
	infos, err := c.ListTools(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, info := range infos {
		name := info.Name
		if _, exists := r.Get(name); exists {
			prefixed := serverName + "_" + name
			fmt.Fprintf(os.Stderr, "mcp: tool %q from server %q collides with an existing tool, registering as %q\n", name, serverName, prefixed)
			info.Name = prefixed
		}
		r.Register(&Tool{c: c, info: info})
		count++
	}
	return count, nil
}
