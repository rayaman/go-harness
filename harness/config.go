package harness

import (
	"fmt"
	"os"
	"time"
)

// Config holds everything needed to point the harness at a local model
// server and control how the agent loop behaves. This is the first place
// to look if you want to change behavior without touching the core loop.
type Config struct {
	// BaseURL is the local model server's OpenAI-compatible endpoint.
	// Common examples:
	//   Ollama:     http://localhost:11434/v1
	//   llama.cpp:  http://localhost:8080/v1
	//   LM Studio:  http://localhost:1234/v1
	//
	// NOTE: the client's rootURL() helper strips a trailing "/v1" to
	// derive the server root, which is what the native Ollama path uses.
	// Keep the "/v1" suffix on BaseURL.
	BaseURL string

	// Provider selects which request path the harness takes:
	//   "ollama": use Ollama's native /api/chat endpoint (supports
	//             image inputs via Message.Images and num_ctx via
	//             MaxContextTokens).
	//   anything else (including ""): use the OpenAI-compatible
	//             chat endpoint. Presets set this explicitly.
	Provider string

	// Model is the model name as known to the local server,
	// e.g. "llama3.1", "qwen2.5:14b", "mistral-nemo".
	// For provider "ollama", use a vision-capable model
	// (e.g. "llama3.2-vision") if you send Message.Images.
	Model string

	// APIKey is sent as a Bearer token in the Authorization header on
	// every request. Leave empty for local servers that don't require
	// auth (e.g. a default Ollama setup). Set it when talking to a
	// hosted/remote OpenAI-compatible API, or a local server you've put
	// behind auth yourself.
	//
	// Don't hardcode real keys here - read them from an environment
	// variable instead (see main.go).
	APIKey string

	// SystemPrompt seeds the conversation with instructions/persona.
	SystemPrompt string

	// Temperature controls sampling randomness (0 = deterministic,
	// higher = more varied/creative).
	Temperature float64

	// MaxToolIterations caps how many rounds of tool-calling the harness
	// will allow before forcing a stop. This guards against the model
	// looping on tool calls forever without ever giving a final answer.
	MaxToolIterations int

	// MaxToolOutputBytes caps the size of a successful tool result before
	// it is fed back to the model (see Harness.runTool): an oversized
	// result is cut to a head and a tail with a "[truncated N of M bytes]"
	// marker in between. Zero (the default) means unlimited, so existing
	// behavior is unchanged unless this is set. Individual tools may
	// override this by implementing the MaxOutputTyper interface.
	MaxToolOutputBytes int

	// MaxParallelTools caps how many concurrency-safe tool calls from a
	// single assistant turn execute at the same time (roadmap 2.1, see
	// Harness.runToolCalls). Zero or negative (the zero value) falls back
	// to the package default (see defaultMaxParallelTools in
	// harness/harness.go: 4). Tools marked serial - those implementing
	// harness.SerialTyper with Serial() == true, and any PolicyAsk tool
	// that isn't marked serial - always run one at a time regardless of
	// this value.
	MaxParallelTools int

	// MaxContextTokens is the model's context window size, in tokens.
	// This is informational by default (the harness uses it to warn when
	// a conversation is getting close to the limit) - see Harness.Ask.
	//
	// IMPORTANT: the OpenAI-compatible chat API has no standard way to
	// set context size - Ollama's docs are explicit about this. The
	// actual window is fixed by the model server, typically via:
	//   - a Modelfile: `PARAMETER num_ctx 32768`, then `ollama create`
	//   - Ollama's native API's "options": {"num_ctx": N} field
	//   - the OLLAMA_CONTEXT_LENGTH environment variable on the server
	// If you leave this at 0, call Client.ContextLength(ctx, model) at
	// startup to read the real value from the server instead of guessing.
	//
	// For provider "ollama", this value is also passed as num_ctx to
	// Client.OllamaChat, so set it to the real window size.
	MaxContextTokens int

	// RequestTimeout caps how long a single model-server HTTP request may
	// take before it fails with a timeout. Zero means "keep the client
	// default" (5 minutes, see harness.NewClient). Workflows set it via
	// config.request_timeout (a Go duration string like "10m" or "300s").
	RequestTimeout time.Duration

	// MaxRetries is the number of EXTRA attempts the chat-completion
	// calls make after a retryable failure (transport errors, HTTP
	// 408/429/5xx): N retries means at most N+1 tries, with exponential
	// backoff (1s, 2s, 4s; see harness/client.go). Zero (the zero value)
	// disables retrying entirely, preserving the original single-attempt
	// behavior; negative also means disabled. Embedders that build a
	// Config by hand therefore keep today's exact behavior; the presets
	// below opt in with MaxRetries: 2.
	MaxRetries int

	// MaxImageDimension is the longest-side pixel cap applied to images
	// attached to chat requests (see utils.LoadImage). Images whose width
	// or height exceed it are downscaled proportionally so the longest
	// side equals the cap; aspect ratio is always preserved and images
	// are never upscaled. Zero (the default) disables scaling entirely:
	// images are sent exactly as they sit on disk, with no re-encode.
	// Workflows set it via config.image_max_dimension (e.g. 2048).
	MaxImageDimension int

	// StreamThinking requests the model server stream its reply (and any
	// thinking/reasoning trace) token-by-token instead of buffering the
	// whole response. The harness forwards deltas to Harness.OnDelta for
	// live terminal printing; the final Ask() result and History stay
	// identical to the non-streaming path. False (the default) keeps the
	// original request/response shape byte-for-byte. The REPL sets it via
	// --stream-thinking; workflows via config.stream_thinking.
	StreamThinking bool

	// CompactThreshold is the fraction of MaxContextTokens at which the
	// oldest History messages get summarized into a single system message
	// before the next request (see Harness.Compact, roadmap 1.1). The
	// auto-trigger in askLoop fires when LastUsage.PromptTokens reaches
	// MaxContextTokens*CompactThreshold, so BOTH this value and
	// MaxContextTokens must be positive for compaction to auto-fire.
	// Zero or negative (the zero value) disables compaction entirely,
	// keeping existing behavior byte-for-byte for embedders that build a
	// Config by hand; the presets below opt in with 0.8. Harness.Compact
	// itself (the manual /compact path) ignores this field.
	CompactThreshold float64

	// CompactKeepTurns is the number of most-recent MESSAGES (not turn
	// pairs) of History kept verbatim when compaction runs; everything
	// older than that window is summarized. Zero or negative falls back
	// to the package default (see defaultCompactKeepTurns in
	// harness/compact.go). The split point never cuts an assistant
	// tool_calls message away from its tool results.
	CompactKeepTurns int

	// Memory attaches a persistent cross-session note store to the
	// Harness (roadmap 2.4, see harness/memory.go): its entries are
	// injected into the leading system message at New/Reset/LoadSession
	// and the remember tool appends to them. Nil (the zero value)
	// disables memory entirely, so embedders and workflows that build a
	// Config by hand stay byte-for-byte unchanged.
	Memory *Memory

	// LogDir enables the JSONL transcript logger (roadmap 3.1, see
	// harness/logging.go): each Harness gets a per-session file
	// transcript-<unixnano>.jsonl in this directory, with one line per
	// chat request, assistant response (with usage when reported) and
	// tool call execution. The directory is created if missing. Empty
	// (the zero value) disables logging entirely, so embedders and
	// workflows that build a Config by hand stay byte-for-byte
	// unchanged. The REPL copies it from the config.yaml key
	// `log-dir` (see harness/config).
	LogDir string
}

// DefaultConfig returns sane defaults for a typical local Ollama setup.
// Override individual fields in main.go to point at a different
// server/model - no need to edit this function itself.
func DefaultConfig() Config {
	return Config{
		Provider:          "openai",
		MaxToolIterations: 100,
		MaxContextTokens:  32768,
		MaxRetries:        2,
		CompactThreshold:  0.8,
		BaseURL:           "http://192.168.1.235:8810/v1",
		Model:             "qwen3-coder-next-gguf",
		SystemPrompt:      "You are a helpful assistant running locally on the user's machine. Use tools when they help you answer accurately, and say so plainly when you don't know something.",
		Temperature:       0.7,
	}
}

// BdoConfig is the preset for the BDO-assistant server. The API key is
// NOT embedded (roadmap 3.2): it comes from the BDO_API_KEY environment
// variable. An unset/empty variable is not fatal - the preset still
// builds and the request simply goes out unauthenticated (fine for
// servers that ignore auth) - but a stderr warning names the variable
// so the auth failure is easy to trace.
func BdoConfig() Config {
	key := os.Getenv("BDO_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "warning: BDO_API_KEY is not set; BdoConfig has no API key")
	}
	return Config{
		Provider:          "openai",
		MaxToolIterations: 50, // might need
		MaxRetries:        2,
		CompactThreshold:  0.8,
		APIKey:            key,
		BaseURL:           "http://192.168.1.235:8810/v1",
		Model:             "Qwen3-4B-Thinking-2507-GGUF",
		SystemPrompt: `You are a helpful assistant running locally on the user's machine to help them with their mmo rpg game bdo.
							Use tools when they help you answer accurately, and say so plainly when you don't know something. 
							When listing items from the market include enhancement and grade with the info the user requested.
							Try differen variations of search queries when looking up things on the bdo market.
							Try to include item details of the items if the user is searching for an item.`,
		Temperature: 0.7,
	}
}

// OllamaConfig returns sane defaults for a default local Ollama setup on
// the native path, which supports image inputs. Note: the client's rootURL()
// helper strips the trailing "/v1" from BaseURL to derive the native
// endpoint, so keep it in the URL.
func OllamaConfig() Config {
	return Config{
		Provider:          "ollama",
		MaxToolIterations: 100,
		MaxContextTokens:  32768, // passed as num_ctx to OllamaChat
		MaxRetries:        2,
		CompactThreshold:  0.8,
		BaseURL:           "http://localhost:11434/v1",
		Model:             "gemma4:31b",
		SystemPrompt:      "You are a helpful assistant running locally on the user's machine. Use tools when they help you answer accurately, and say so plainly when you don't know something.",
		Temperature:       0.7,
	}
}

func StrataConfig() Config {
	return Config{
		Provider:          "openai",
		MaxToolIterations: 100,
		MaxContextTokens:  128000, // passed as num_ctx to OllamaChat
		MaxRetries:        3,
		CompactThreshold:  0.8,
		APIKey:            "myapikey",
		BaseURL:           "http://192.168.1.235:8095/v1",
		Model:             "qwen3.8-flash-next-iq3_s",
		SystemPrompt:      "You are a helpful assistant running locally on the user's machine. Use tools when they help you answer accurately, and say so plainly when you don't know something.",
		Temperature:       0.7,
	}
}
