package harness

// Config holds everything needed to point the harness at a local model
// server and control how the agent loop behaves. This is the first place
// to look if you want to change behavior without touching the core loop.
type Config struct {
	// BaseURL is the local model server's OpenAI-compatible endpoint.
	// Common examples:
	//   Ollama:     http://localhost:11434/v1
	//   llama.cpp:  http://localhost:8080/v1
	//   LM Studio:  http://localhost:1234/v1
	BaseURL string

	// Model is the model name as known to the local server,
	// e.g. "llama3.1", "qwen2.5:14b", "mistral-nemo".
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
	MaxContextTokens int
}

// DefaultConfig returns sane defaults for a typical local Ollama setup.
// Override individual fields in main.go to point at a different
// server/model - no need to edit this function itself.
func DefaultConfig() Config {
	return Config{
		BaseURL:           "http://192.168.1.235:8810/v1",
		Model:             "qwen3-coder-next-gguf",
		SystemPrompt:      "You are a helpful assistant running locally on the user's machine. Use tools when they help you answer accurately, and say so plainly when you don't know something.",
		Temperature:       0.7,
		MaxToolIterations: 100,
	}
}

func TestConfig() Config {
	return Config{
		BaseURL:           "http://192.168.1.235:8810/v1",
		Model:             "qwen3-coder-next-gguf",
		SystemPrompt:      "You are a helpful assistant running locally on the user's machine. Use tools when they help you answer accurately, and say so plainly when you don't know something.",
		Temperature:       0.7,
		MaxToolIterations: 100,
		APIKey:            "sk-unsloth-94d524b13d50f995c58cbb5d08c090c3",
	}
}
