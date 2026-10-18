package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a thin wrapper around a local model server's chat-completions
// endpoint. It deliberately knows nothing about tools/agent-loop logic -
// it just sends a request and parses the response. If your local server
// speaks a different protocol, this is the only file you need to rewrite;
// everything else in the package just calls ChatCompletion.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient builds a Client pointed at baseURL (e.g. "http://localhost:11434/v1").
// Pass an empty apiKey for local servers that don't require auth.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			// Local inference can be slow, especially for the first token
			// on a big prompt or a large model, so give it plenty of room.
			// Lower this if you want faster failure on a hung server.
			Timeout: 5 * time.Minute,
		},
	}
}

// SetRequestTimeout overrides the per-request HTTP timeout for model-server
// calls. d <= 0 is a no-op, so the NewClient default (5 minutes) stays the
// fallback when no request_timeout is configured.
func (c *Client) SetRequestTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	c.httpClient.Timeout = d
}

// ChatCompletion sends the full conversation (plus optional tool schemas)
// to the model server and returns its parsed response.
func (c *Client) ChatCompletion(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		// Most OpenAI-compatible APIs (including hosted ones) expect a
		// Bearer token. If yours uses a different header - e.g. some
		// gateways want "x-api-key" - just change this one line.
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request to model server failed (is it running at %s?): %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model server returned %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("parse response: %w (body: %s)", err, string(respBody))
	}
	return &chatResp, nil
}

// ChatCompletionStream is ChatCompletion with "stream": true: instead of one
// JSON body the server replies with an SSE stream of "data: {...}" chunk
// lines, terminated by "data: [DONE]". onDelta (may be nil) is called per
// chunk with whatever reply/thinking text that chunk carried. The fully
// accumulated reply - content, reasoning_content, and tool_calls reassembled
// from their deltas - is returned in the same ChatResponse shape the
// non-streaming path produces, so callers can reuse it unchanged for
// History and usage.
func (c *Client) ChatCompletionStream(ctx context.Context, req ChatRequest, onDelta func(ChatDelta)) (*ChatResponse, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request to model server failed (is it running at %s?): %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("model server returned %d: %s", resp.StatusCode, string(respBody))
	}

	var (
		content   strings.Builder
		reasoning strings.Builder
		finish    string
		usage     Usage
		toolAcc   = map[int]*ToolCall{}
		toolOrder []int
	)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue // blank keep-alive lines, SSE comments, "event:" headers
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Role             string `json:"role"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					// Ollama's /v1 shim spells thinking "thinking" in stream
					// deltas where llama.cpp-style servers use
					// "reasoning_content"; accept either.
					Thinking  string `json:"thinking"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			// Usage only shows up when the server opts into sending it
			// on streams (OpenAI's stream_options.include_usage); absent
			// elsewhere, so keep the zero value.
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil, fmt.Errorf("parse stream chunk: %w (chunk: %s)", err, payload)
		}
		if chunk.Usage != nil && chunk.Usage.TotalTokens != 0 {
			usage = *chunk.Usage
		}

		for _, ch := range chunk.Choices {
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
			if ch.Delta.Content != "" {
				content.WriteString(ch.Delta.Content)
				if onDelta != nil {
					onDelta(ChatDelta{Content: ch.Delta.Content})
				}
			}
			if thinking := ch.Delta.ReasoningContent + ch.Delta.Thinking; thinking != "" {
				reasoning.WriteString(thinking)
				if onDelta != nil {
					onDelta(ChatDelta{ReasoningContent: thinking})
				}
			}
			// Tool calls stream as fragments keyed by index: the first
			// fragment carries id/type/name, later ones append argument
			// pieces. Reassemble into whole ToolCalls so the agent loop
			// below askLoop's dispatch sees the same shape as
			// non-streaming.
			for _, frag := range ch.Delta.ToolCalls {
				acc, ok := toolAcc[frag.Index]
				if !ok {
					acc = &ToolCall{Type: "function"}
					toolAcc[frag.Index] = acc
					toolOrder = append(toolOrder, frag.Index)
				}
				if frag.ID != "" {
					acc.ID = frag.ID
				}
				acc.Function.Name += frag.Function.Name
				acc.Function.Arguments += frag.Function.Arguments
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	var toolCalls []ToolCall
	for _, idx := range toolOrder {
		call := toolAcc[idx]
		if call.ID == "" {
			// Some servers omit IDs on streamed calls; the loop links
			// results by this ID, so synthesize a stable one.
			call.ID = fmt.Sprintf("stream_call_%d", idx)
		}
		if call.Function.Arguments == "" {
			call.Function.Arguments = "{}"
		}
		toolCalls = append(toolCalls, *call)
	}

	return &ChatResponse{
		Choices: []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		}{
			{
				Message: Message{
					Role:             "assistant",
					Content:          content.String(),
					ReasoningContent: reasoning.String(),
					ToolCalls:        toolCalls,
				},
				FinishReason: finish,
			},
		},
		Usage: usage,
	}, nil
}

// ollamaArgumentsToObject parses a FunctionCall.Arguments JSON string back
// into the object form Ollama's native API expects. Malformed arguments
// degrade to {} so the request still goes out.
func ollamaArgumentsToObject(arguments string) map[string]any {
	var args map[string]any
	if arguments != "" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return map[string]any{}
		}
	}
	return args
}

// ollamaMessages converts wire Messages into Ollama's native /api/chat
// message shape {role, content, images?, tool_calls?}. Images flow through
// as base64 without a data-URI prefix, which is what native Ollama wants.
// Tool-call arguments are re-parsed from the JSON string we carry on the
// wire back into the object form native expects, so multi-turn tool loops
// round-trip correctly.
func ollamaMessages(messages []Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		m := map[string]any{"role": msg.Role, "content": ExtractText(msg.Content)}
		if len(msg.Images) > 0 {
			m["images"] = msg.Images
		}
		if len(msg.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(msg.ToolCalls))
			for _, call := range msg.ToolCalls {
				calls = append(calls, map[string]any{
					"function": map[string]any{
						"name":      call.Function.Name,
						"arguments": ollamaArgumentsToObject(call.Function.Arguments),
					},
				})
			}
			m["tool_calls"] = calls
		}
		out = append(out, m)
	}
	return out
}

// ollamaChatBody builds the /api/chat request body shared by the streaming
// and non-streaming native calls. Pass stream=false/think=false for the
// classic call: the body stays byte-for-byte what it always was.
func ollamaChatBody(req ChatRequest, numCtx int, stream, think bool) map[string]any {
	body := map[string]any{
		"model":    req.Model,
		"messages": ollamaMessages(req.Messages),
		"stream":   stream,
	}
	if think {
		body["think"] = true
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	options := map[string]any{}
	if numCtx > 0 {
		options["num_ctx"] = numCtx
	}
	if req.Temperature != 0 {
		options["temperature"] = req.Temperature
	}
	if len(options) > 0 {
		body["options"] = options
	}
	return body
}

// ollamaToolCall is one entry of Ollama's native message.tool_calls array
// (field name is plural, unlike the OpenAI-compat path's singular
// "tool_call"). Shared by the non-streaming and streaming native parses.
type ollamaToolCall struct {
	Function struct {
		Name      string `json:"name"`
		Arguments any    `json:"arguments"`
	} `json:"function"`
}

// convertOllamaToolCalls maps native tool_calls onto the wire ToolCall
// shape. Arguments arrive as an object (native) rather than the JSON string
// the wire type carries, so they are re-serialized; missing arguments
// degrade to "{}".
func convertOllamaToolCalls(calls []ollamaToolCall) ([]ToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]ToolCall, 0, len(calls))
	for i, call := range calls {
		arguments := "{}"
		if call.Function.Arguments != nil {
			b, err := json.Marshal(call.Function.Arguments)
			if err != nil {
				return nil, fmt.Errorf("serialize tool-call arguments: %w", err)
			}
			arguments = string(b)
		}
		// Native Ollama links tool calls to results by order, not by ID, so
		// synthesized IDs here are only placeholders for the wire type.
		out = append(out, ToolCall{
			ID:       fmt.Sprintf("ollama_call_%d", i),
			Type:     "function",
			Function: FunctionCall{Name: call.Function.Name, Arguments: arguments},
		})
	}
	return out, nil
}

// OllamaChat sends the conversation to Ollama's native /api/chat endpoint
// (no "/v1" prefix, unlike the OpenAI-compat path) and maps the native
// response back into the wire ChatResponse shape so the agent loop stays
// unchanged. Pass numCtx <= 0 to keep the server's default context window;
// harness.go passes Config.MaxContextTokens.
func (c *Client) OllamaChat(ctx context.Context, req ChatRequest, numCtx int) (*ChatResponse, error) {
	respBody, err := c.doJSON(ctx, http.MethodPost, c.rootURL()+"/api/chat",
		ollamaChatBody(req, numCtx, false, false))
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			// Ollama's name for the model's reasoning trace (qwen3,
			// deepseek-r1, etc). Keep it on the wire message instead of
			// silently dropping it.
			Thinking  string           `json:"thinking"`
			ToolCalls []ollamaToolCall `json:"tool_calls"`
		} `json:"message"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse native response: %w (body: %s)", err, string(respBody))
	}

	toolCalls, err := convertOllamaToolCalls(parsed.Message.ToolCalls)
	if err != nil {
		return nil, err
	}

	return &ChatResponse{
		Choices: []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		}{
			{
				Message: Message{
					Role:             parsed.Message.Role,
					Content:          parsed.Message.Content,
					ReasoningContent: parsed.Message.Thinking,
					ToolCalls:        toolCalls,
				},
				FinishReason: "stop",
			},
		},
	}, nil
}

// OllamaChatStream is OllamaChat with Ollama's native streaming: "stream":
// true makes /api/chat reply with newline-delimited JSON objects, one per
// generated chunk, the last carrying "done": true (plus eval counts, which
// we map onto Usage). onDelta (may be nil) receives content and thinking
// deltas as they arrive.
//
// Deviation note on the "think" flag: Ollama's documented way to ask a
// thinking-capable model for a reasoning trace on the native API is the
// top-level "think": true field, but that field only exists on Ollama
// >= v0.9. This path is only reached when StreamThinking is enabled, so we
// always send it: servers that predate the field ignore unknown keys and
// models that emit thinking by default (deepseek-r1 and friends) populate
// message.thinking either way, which we parse regardless of which path
// produced it.
func (c *Client) OllamaChatStream(ctx context.Context, req ChatRequest, numCtx int, onDelta func(ChatDelta)) (*ChatResponse, error) {
	bodyBytes, err := json.Marshal(ollamaChatBody(req, numCtx, true, true))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.rootURL()+"/api/chat", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request to model server failed (is it running at %s?): %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("model server returned %d: %s", resp.StatusCode, string(respBody))
	}

	var (
		content   strings.Builder
		reasoning strings.Builder
		toolCalls []ToolCall
		usage     Usage
	)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk struct {
			Message struct {
				Role      string           `json:"role"`
				Content   string           `json:"content"`
				Thinking  string           `json:"thinking"`
				ToolCalls []ollamaToolCall `json:"tool_calls"`
			} `json:"message"`
			Done            bool `json:"done"`
			PromptEvalCount int  `json:"prompt_eval_count"`
			EvalCount       int  `json:"eval_count"`
		}
		if err := json.Unmarshal(line, &chunk); err != nil {
			return nil, fmt.Errorf("parse native stream chunk: %w (chunk: %s)", err, line)
		}
		if chunk.Message.Content != "" {
			content.WriteString(chunk.Message.Content)
			if onDelta != nil {
				onDelta(ChatDelta{Content: chunk.Message.Content})
			}
		}
		if chunk.Message.Thinking != "" {
			reasoning.WriteString(chunk.Message.Thinking)
			if onDelta != nil {
				onDelta(ChatDelta{ReasoningContent: chunk.Message.Thinking})
			}
		}
		if len(chunk.Message.ToolCalls) > 0 {
			// Ollama delivers whole tool calls in one chunk (no argument
			// fragments like the OpenAI-compat path), so a straight map
			// is enough.
			converted, err := convertOllamaToolCalls(chunk.Message.ToolCalls)
			if err != nil {
				return nil, err
			}
			toolCalls = append(toolCalls, converted...)
		}
		if chunk.Done {
			if chunk.PromptEvalCount != 0 || chunk.EvalCount != 0 {
				usage = Usage{
					PromptTokens:     chunk.PromptEvalCount,
					CompletionTokens: chunk.EvalCount,
					TotalTokens:      chunk.PromptEvalCount + chunk.EvalCount,
				}
			}
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	return &ChatResponse{
		Choices: []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		}{
			{
				Message: Message{
					Role:             "assistant",
					Content:          content.String(),
					ReasoningContent: reasoning.String(),
					ToolCalls:        toolCalls,
				},
				FinishReason: "stop",
			},
		},
		Usage: usage,
	}, nil
}

// rootURL strips a trailing "/v1" so we can also reach native endpoints
// (like Ollama's /api/show or llama.cpp's /props) at the same host as the
// OpenAI-compatible chat endpoint. If your BaseURL doesn't end in "/v1"
// this is a no-op.
func (c *Client) rootURL() string {
	return strings.TrimSuffix(c.baseURL, "/v1")
}

// doJSON is a small shared helper for the auxiliary endpoints below
// (model listing, context-length lookups). It applies the same auth
// header as ChatCompletion and returns the raw response body. Pass a nil
// reqBody for a GET with no body.
func (c *Client) doJSON(ctx context.Context, method, url string, reqBody any) ([]byte, error) {
	var reader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response from %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d: %s", url, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// ModelInfo describes one model as reported by the model server.
type ModelInfo struct {
	ID         string `json:"id"`
	ModifiedAt string `json:"modified_at,omitempty"`
	SizeBytes  int64  `json:"size,omitempty"`
}

// ListModels asks the model server which models it currently serves. It
// tries the OpenAI-style GET /v1/models first (llama.cpp, Unsloth, vLLM,
// and Ollama's own /v1 shim) and falls back to Ollama's native GET
// /api/tags. Servers that implement neither return an error.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if models, err := c.listModelsFromOpenAI(ctx); err == nil {
		return models, nil
	}
	return c.listModelsFromOllamaTags(ctx)
}

func (c *Client) listModelsFromOpenAI(ctx context.Context) ([]ModelInfo, error) {
	body, err := c.doJSON(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	// /v1/models entries differ by server; llama.cpp/Unsloth add "meta"
	// with size/timestamps, while Ollama's shim only fills "id" and
	// "created". Decode the union and keep what's present.
	var parsed struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
			Meta    struct {
				SizeBytes  int64  `json:"size_bytes"`
				ModifiedAt string `json:"modified_at"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse /v1/models response: %w", err)
	}
	out := make([]ModelInfo, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		info := ModelInfo{ID: m.ID, ModifiedAt: m.Meta.ModifiedAt, SizeBytes: m.Meta.SizeBytes}
		if info.ModifiedAt == "" && m.Created > 0 {
			info.ModifiedAt = time.Unix(m.Created, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, info)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty model list from /v1/models")
	}
	return out, nil
}

func (c *Client) listModelsFromOllamaTags(ctx context.Context) ([]ModelInfo, error) {
	body, err := c.doJSON(ctx, http.MethodGet, c.rootURL()+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Models []struct {
			Name       string `json:"name"`
			ModifiedAt string `json:"modified_at"`
			Size       int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse /api/tags response: %w", err)
	}
	out := make([]ModelInfo, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		out = append(out, ModelInfo{ID: m.Name, ModifiedAt: m.ModifiedAt, SizeBytes: m.Size})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty model list from /api/tags")
	}
	return out, nil
}

// ContextLength asks the model server for a model's maximum context
// window, in tokens. There's no standard endpoint for this across
// servers, so it tries each known convention in turn and returns the
// first one that succeeds:
//
//  1. OpenAI-style GET /v1/models - works for llama.cpp / llama-server
//     based servers, including Unsloth's serving mode ("meta":{"n_ctx":N}).
//  2. llama.cpp/Unsloth's native GET /props
//     ("default_generation_settings":{"n_ctx":N}).
//  3. Ollama's native POST /api/show ("model_info":{"<family>.context_length":N}).
//
// If none of these match your server, this returns an error - set
// Config.MaxContextTokens manually as a fallback.
func (c *Client) ContextLength(ctx context.Context, model string) (int, error) {
	if n, err := c.contextLengthFromModelsEndpoint(ctx, model); err == nil {
		return n, nil
	}
	if n, err := c.contextLengthFromProps(ctx); err == nil {
		return n, nil
	}
	if n, err := c.contextLengthFromOllamaShow(ctx, model); err == nil {
		return n, nil
	}
	return 0, fmt.Errorf("no known endpoint reported a context length for %q", model)
}

// contextLengthFromModelsEndpoint covers llama.cpp/llama-server-based
// servers (including Unsloth), which report per-model context size via
// the standard OpenAI /v1/models listing.
func (c *Client) contextLengthFromModelsEndpoint(ctx context.Context, model string) (int, error) {
	body, err := c.doJSON(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return 0, err
	}

	var parsed struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				NCtx int `json:"n_ctx"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("parse /v1/models response: %w", err)
	}
	for _, m := range parsed.Data {
		if m.Meta.NCtx > 0 && (m.ID == model || len(parsed.Data) == 1) {
			return m.Meta.NCtx, nil
		}
	}
	return 0, fmt.Errorf("no context length in /v1/models response")
}

// contextLengthFromProps covers llama.cpp/llama-server's native /props
// endpoint, which Unsloth also exposes since it runs on llama-server.
func (c *Client) contextLengthFromProps(ctx context.Context) (int, error) {
	body, err := c.doJSON(ctx, http.MethodGet, c.rootURL()+"/props", nil)
	if err != nil {
		return 0, err
	}

	var parsed struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("parse /props response: %w", err)
	}
	if parsed.DefaultGenerationSettings.NCtx > 0 {
		return parsed.DefaultGenerationSettings.NCtx, nil
	}
	return 0, fmt.Errorf("no context length in /props response")
}

// contextLengthFromOllamaShow covers Ollama's native /api/show endpoint.
func (c *Client) contextLengthFromOllamaShow(ctx context.Context, model string) (int, error) {
	body, err := c.doJSON(ctx, http.MethodPost, c.rootURL()+"/api/show", map[string]string{"model": model})
	if err != nil {
		return 0, err
	}

	// model_info keys are prefixed with the model family, e.g.
	// "llama.context_length" or "gemma4.context_length" - the prefix
	// varies per model, so we scan for the suffix instead of a fixed key.
	var parsed struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("parse /api/show response: %w", err)
	}
	for key, val := range parsed.ModelInfo {
		if strings.HasSuffix(key, ".context_length") {
			if n, ok := val.(float64); ok {
				return int(n), nil
			}
		}
	}
	return 0, fmt.Errorf("context_length not found in model_info for %q", model)
}
