package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"ai-harness/harness/utils"
)

// Harness ties together a Client (talks to the model server), a Registry
// (tools the model may call), and the running conversation History.
//
// This is the core "agent loop":
//
//  1. Send the conversation so far (+ available tools) to the model.
//  2. If the model responds with plain text -> done, return it.
//  3. If the model responds with tool call(s) -> run them, append the
//     results to History as "tool" messages, and go back to step 1 so
//     the model can use those results to form its next response.
//
// This repeats until the model gives a final answer or MaxToolIterations
// is hit (a safety valve against infinite tool-calling loops).
type Harness struct {
	Config   Config
	Client   *Client
	Registry *Registry
	History  []Message

	// LastUsage holds the token counts from the most recent chat
	// completion call. Not every server populates this (it's
	// best-effort), so check for a zero value before relying on it.
	LastUsage Usage
}

// New creates a Harness and seeds the conversation with the system prompt.
func New(cfg Config, client *Client, registry *Registry) *Harness {
	h := &Harness{
		Config:   cfg,
		Client:   client,
		Registry: registry,
	}
	if cfg.SystemPrompt != "" {
		h.History = append(h.History, Message{Role: "system", Content: cfg.SystemPrompt})
	}
	return h
}

// Ask sends a user message through the full agent loop (including any
// back-and-forth tool calls) and returns the model's final text reply.
// This is the main entry point to call for each turn of conversation -
// from a CLI REPL, a web handler, a scheduled job, etc.
func (h *Harness) Ask(ctx context.Context, userInput string) (string, error) {
	return h.askLoop(ctx, userInput, nil)
}

// AskWithImages is Ask() with image files attached to the user message
// (paths to png/jpg/jpeg/gif/webp on disk). Use it instead of Ask when the
// turn needs vision input; it runs the exact same agent loop, including any
// tool calls the model decides to make.
func (h *Harness) AskWithImages(ctx context.Context, input string, imagePaths []string) (string, error) {
	return h.askLoop(ctx, input, imagePaths)
}

// buildUserMessage assembles the user Message for this turn, attaching any
// images in the shape the configured provider expects:
//
//   - Provider "ollama": Content stays a plain string and the raw base64
//     blobs go on Message.Images, since Ollama's native /api/chat takes
//     images: [base64...] with no data-URI prefix.
//   - Anything else: the OpenAI-compat multimodal shape - Content becomes a
//     []ContentPart holding one text part plus one image_url part per
//     image, where the URL is a "data:<mime>;base64,<b64>" URI.
//
// An image file that fails to load is reported on stderr and skipped: the
// prompt still goes out with whatever images did load, rather than
// discarding the whole turn over one bad path.
func (h *Harness) buildUserMessage(input string, imagePaths []string) Message {
	if h.Config.Provider == "ollama" {
		// Declared as a nil slice on purpose: with no images it stays nil
		// (never an empty slice), so json.Marshal omits "images" entirely
		// from the request body.
		var images []string
		for _, path := range imagePaths {
			b64, _, err := utils.LoadImage(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: skipping image: %v\n", err)
				continue
			}
			images = append(images, b64)
		}
		return Message{Role: "user", Content: input, Images: images}
	}

	textContent := input
	parts := []ContentPart{{Type: "text", Text: &textContent}}
	for _, path := range imagePaths {
		b64, mime, err := utils.LoadImage(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping image: %v\n", err)
			continue
		}
		// Text stays nil on image parts and ImageURL stays nil on the text
		// part, so each part marshals with only the key that applies to it.
		parts = append(parts, ContentPart{
			Type:     "image_url",
			ImageURL: &ImageURL{URL: "data:" + mime + ";base64," + b64},
		})
	}

	// No images were requested, or none of them loaded: a bare string
	// Content is what every OpenAI-compat server accepts, so fall back to
	// that instead of sending a lone text part.
	if len(parts) == 1 {
		return Message{Role: "user", Content: input}
	}
	return Message{Role: "user", Content: parts}
}

// askLoop is the agent loop shared by Ask and AskWithImages: append the
// user message (with images, if any), then keep trading tool calls and
// results until the model returns a final text answer.
func (h *Harness) askLoop(ctx context.Context, userInput string, imagePaths []string) (string, error) {
	h.History = append(h.History, h.buildUserMessage(userInput, imagePaths))

	for i := 0; i < h.Config.MaxToolIterations; i++ {
		chatReq := ChatRequest{
			Model:       h.Config.Model,
			Messages:    h.History,
			Tools:       h.Registry.Schemas(),
			Temperature: h.Config.Temperature,
			Stream:      false,
		}

		// Provider dispatch: "ollama" goes to the native /api/chat endpoint
		// (which accepts images and lets us set num_ctx, so the context
		// window isn't left to the server's default); every other provider
		// uses the OpenAI-compatible chat-completions endpoint. Both return
		// the same ChatResponse shape, so everything below is shared.
		var resp *ChatResponse
		if h.Config.Provider == "ollama" {
			ollamaResp, err := h.Client.OllamaChat(ctx, chatReq, h.Config.MaxContextTokens)
			if err != nil {
				return "", fmt.Errorf("ollama chat: %w", err)
			}
			resp = ollamaResp
		} else {
			openaiResp, err := h.Client.ChatCompletion(ctx, chatReq)
			if err != nil {
				return "", fmt.Errorf("chat completion: %w", err)
			}
			resp = openaiResp
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("model server returned no choices")
		}

		assistantMsg := resp.Choices[0].Message
		h.History = append(h.History, assistantMsg)
		h.LastUsage = resp.Usage

		// No tool calls -> the model is giving its final answer.
		if len(assistantMsg.ToolCalls) == 0 {
			return ExtractText(assistantMsg.Content), nil
		}

		// Otherwise, run each requested tool and feed the results back
		// into the conversation, then loop so the model can respond to them.
		for _, call := range assistantMsg.ToolCalls {
			result := h.runTool(call)
			h.History = append(h.History, Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: call.ID,
				Name:       call.Function.Name,
			})
		}
	}

	return "", fmt.Errorf("hit max tool iterations (%d) without a final answer", h.Config.MaxToolIterations)
}

// runTool dispatches a single tool call. Errors are stringified rather
// than returned to the caller, because the intended recipient of an error
// here is the MODEL (so it can see what went wrong and try again), not the
// Go caller of Ask().
//
// This is also the natural place to add a human-confirmation gate for
// risky tools, e.g.:
//
//	if call.Function.Name == "run_shell_command" && !confirmWithUser(call) {
//	    return "user declined to run this tool"
//	}
func (h *Harness) runTool(call ToolCall) string {
	tool, ok := h.Registry.Get(call.Function.Name)
	if !ok {
		return fmt.Sprintf("error: no such tool %q", call.Function.Name)
	}

	result, err := tool.Execute(json.RawMessage(call.Function.Arguments))
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return result
}
