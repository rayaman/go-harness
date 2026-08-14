package harness

import (
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
