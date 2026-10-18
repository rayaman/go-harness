package harness

import "time"

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
}

// DefaultConfig returns sane defaults for a typical local Ollama setup.
// Override individual fields in main.go to point at a different
// server/model - no need to edit this function itself.
func DefaultConfig() Config {
	return Config{
		Provider:          "openai",
		MaxToolIterations: 100,
		MaxContextTokens:  32768,
		BaseURL:           "http://192.168.1.235:8810/v1",
		Model:             "qwen3-coder-next-gguf",
		SystemPrompt:      "You are a helpful assistant running locally on the user's machine. Use tools when they help you answer accurately, and say so plainly when you don't know something.",
		Temperature:       0.7,
	}
}

func BdoConfig() Config {
	return Config{
		Provider:          "openai",
		MaxToolIterations: 50, // might need
		APIKey:            "sk-unsloth-94d524b13d50f995c58cbb5d08c090c3",
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
		BaseURL:           "http://localhost:11434/v1",
		Model:             "gemma4:31b",
		SystemPrompt:      "You are a helpful assistant running locally on the user's machine. Use tools when they help you answer accurately, and say so plainly when you don't know something.",
		Temperature:       0.7,
	}
}
