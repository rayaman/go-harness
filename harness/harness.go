package harness

import (
	"context"
	"encoding/json"
	"fmt"
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
	h.History = append(h.History, Message{Role: "user", Content: userInput})

	for i := 0; i < h.Config.MaxToolIterations; i++ {
		resp, err := h.Client.ChatCompletion(ctx, ChatRequest{
			Model:       h.Config.Model,
			Messages:    h.History,
			Tools:       h.Registry.Schemas(),
			Temperature: h.Config.Temperature,
			Stream:      false,
		})
		if err != nil {
			return "", fmt.Errorf("chat completion: %w", err)
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("model server returned no choices")
		}

		assistantMsg := resp.Choices[0].Message
		h.History = append(h.History, assistantMsg)
		h.LastUsage = resp.Usage

		// No tool calls -> the model is giving its final answer.
		if len(assistantMsg.ToolCalls) == 0 {
			return assistantMsg.Content, nil
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
