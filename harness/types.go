package harness

// This file defines the "wire format" the harness speaks. It matches the
// OpenAI-style /v1/chat/completions schema, which is what most local model
// servers implement (Ollama, llama.cpp server, LM Studio, vLLM, etc).
// If your server uses a different shape, this is the only file you should
// need to change.

// Message represents a single message in the conversation.
type Message struct {
	Role    string `json:"role"` // "system", "user", "assistant", or "tool"
	Content string `json:"content"`

	// ToolCalls is set on assistant messages when the model wants to call
	// one or more tools instead of (or before) answering directly.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolCallID and Name are set on "tool" role messages - they link a
	// tool's result back to the specific call the model made.
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// ToolCall is a single function/tool invocation requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function" in this schema
	Function FunctionCall `json:"function"`
}

// FunctionCall carries the tool name and its arguments, encoded as a raw
// JSON string (per the OpenAI schema) rather than a nested object.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string, e.g. `{"path":"foo.txt"}`
}

// ChatRequest is the body sent to the model server's chat-completions endpoint.
type ChatRequest struct {
	Model       string       `json:"model"`
	Messages    []Message    `json:"messages"`
	Tools       []ToolSchema `json:"tools,omitempty"`
	Temperature float64      `json:"temperature,omitempty"`
	Stream      bool         `json:"stream"`
}

// ToolSchema is how a tool is described *to the model* so it knows the
// tool exists, what it does, and what arguments it expects.
type ToolSchema struct {
	Type     string       `json:"type"` // "function"
	Function FunctionSpec `json:"function"`
}

// FunctionSpec is the descriptive part of a ToolSchema.
type FunctionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema object
}

// Usage reports token counts for a single chat completion call, as
// returned by most OpenAI-compatible servers (including Ollama's /v1
// endpoint). PromptTokens is effectively "how much context this call
// used" - the conversation history plus tool schemas you sent in.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is the (simplified) response shape from the model server.
// We only pull out the fields the harness actually needs.
type ChatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}
