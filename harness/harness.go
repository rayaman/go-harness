package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"unicode/utf8"

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

	// Confirm gates tools whose Registry policy is PolicyAsk (roadmap
	// 1.2): it is called with the pending ToolCall just before Execute
	// and returning false feeds a "user declined" message back to the
	// model instead of running the tool. Nil (the default) means
	// auto-allow, preserving existing embedder behavior.
	Confirm func(call ToolCall) bool

	// OnDelta is called with streamed model output when
	// Config.StreamThinking is enabled: kind is "thinking" for the
	// model's reasoning trace or "content" for the reply text, with the
	// delta's raw text. Set it after New (like Registry above - it's
	// plain mutable state, not constructor config). Never called on the
	// non-streaming path, and Ask's return value / History are the same
	// either way.
	OnDelta func(kind, text string)

	// Memory is the persistent cross-session note store (roadmap 2.4).
	// Nil (the zero value / Config.Memory left nil) disables memory
	// entirely: New/Reset/LoadSession then behave byte-for-byte as they
	// did before this feature. Set it via Config.Memory in New, or
	// assign directly after construction.
	Memory *Memory

	// transcript is the JSONL transcript logger (roadmap 3.1), opened
	// by New when Config.LogDir is set. Nil (the default - empty
	// LogDir) disables logging entirely, so every hook below is a
	// no-op and existing behavior is byte-for-byte unchanged.
	transcript *transcriptLogger

	// systemBase is the raw system prompt WITHOUT any injected memory
	// block. New/Reset/LoadSession capture it there and applyMemory
	// rebuilds History[0].Content as systemBase + block, so re-running
	// injection (remember, Reset, session load) never stacks blocks.
	systemBase string
}

// New creates a Harness and seeds the conversation with the system prompt.
// It also pushes cfg.MaxRetries onto the Client (0/negative = no retrying,
// so a zero-value Config keeps the client's single-attempt behavior).
// With cfg.Memory set (roadmap 2.4), the memory store is attached and its
// current block is injected into the seeded system message.
func New(cfg Config, client *Client, registry *Registry) *Harness {
	client.SetMaxRetries(cfg.MaxRetries)
	h := &Harness{
		Config:   cfg,
		Client:   client,
		Registry: registry,
		Memory:   cfg.Memory,
	}
	// Transcript logging (roadmap 3.1): empty LogDir = disabled and
	// nothing touches the disk; a logger that fails to open is a
	// startup warning, never a New failure (logging is best-effort).
	if tl, err := newTranscriptLogger(cfg.LogDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: transcript logging disabled: %v\n", err)
	} else {
		h.transcript = tl
	}
	if cfg.SystemPrompt != "" {
		h.History = append(h.History, Message{Role: "system", Content: cfg.SystemPrompt})
	}
	h.systemBase = cfg.SystemPrompt
	// Memory read failures are warning-only inside applyMemory, so New
	// never fails over a broken memory file.
	_ = h.applyMemory()
	return h
}

// Reset starts a new conversation on the same Harness: History is
// re-seeded exactly the way New seeds it (just the system message when
// Config.SystemPrompt is set, otherwise empty), while Config, Client,
// Registry and the Confirm/OnDelta hooks all stay as they are. This is
// the /reset command behind the REPL (roadmap 1.3). With Memory set,
// systemBase is re-captured from Config.SystemPrompt and the current
// memory block is re-injected, so /reset keeps memory (roadmap 2.4).
func (h *Harness) Reset() {
	h.History = nil
	if h.Config.SystemPrompt != "" {
		h.History = append(h.History, Message{Role: "system", Content: h.Config.SystemPrompt})
	}
	h.systemBase = h.Config.SystemPrompt
	_ = h.applyMemory()
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
			b64, _, err := utils.LoadImageScaled(path, h.Config.MaxImageDimension)
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
		b64, mime, err := utils.LoadImageScaled(path, h.Config.MaxImageDimension)
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
//
// Cancellation safety (roadmap 3.3): if the loop fails because ctx was
// cancelled, History is rolled back to exactly its pre-Ask state (the
// snapshot below). runToolCalls already keeps tool_calls/tool pairs
// together even when cancelled, so this is belt-and-braces: it also
// drops the half-finished turn's user and assistant messages, leaving
// the session pristine for the next prompt. The rollback only fires on
// context cancellation - ordinary (non-cancel) errors keep today's
// behavior byte-for-byte.
func (h *Harness) askLoop(ctx context.Context, userInput string, imagePaths []string) (string, error) {
	snapshot := len(h.History)
	answer, err := h.runAskLoop(ctx, userInput, imagePaths)
	if err != nil && ctx.Err() != nil {
		h.History = h.History[:snapshot]
	}
	return answer, err
}

// runAskLoop is the un-cancelled-rollback body of askLoop (see there).
func (h *Harness) runAskLoop(ctx context.Context, userInput string, imagePaths []string) (string, error) {
	h.History = append(h.History, h.buildUserMessage(userInput, imagePaths))

	for i := 0; i < h.Config.MaxToolIterations; i++ {
		// Auto compaction (roadmap 1.1): when the previous response's
		// prompt usage crossed CompactThreshold of MaxContextTokens,
		// summarize the oldest History before building this request. At
		// most ONE attempt per iteration: if it changed History, the
		// usage re-check at the top of the next iteration decides
		// whether a second is needed (and a second Compact returning
		// false naturally stops the loop). A failure is warning-only:
		// the turn proceeds with the oversized context rather than
		// losing the whole turn over a failed summary. Deliberately not
		// wired into forceFinalAnswer: the forced final answer must
		// stay a single request.
		if h.Config.CompactThreshold > 0 && h.Config.MaxContextTokens > 0 &&
			h.LastUsage.PromptTokens >= int(float64(h.Config.MaxContextTokens)*h.Config.CompactThreshold) {
			if _, err := h.Compact(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "warning: context compaction failed: %v\n", err)
			}
		}

		chatReq := ChatRequest{
			Model:       h.Config.Model,
			Messages:    h.History,
			Tools:       h.Registry.Schemas(),
			Temperature: h.Config.Temperature,
			Stream:      false,
		}
		// Transcript logging (roadmap 3.1): one line per outbound
		// chat request; no-op when Config.LogDir is unset.
		h.logRequest(chatReq)

		// Provider dispatch: "ollama" goes to the native /api/chat endpoint
		// (which accepts images and lets us set num_ctx, so the context
		// window isn't left to the server's default); every other provider
		// uses the OpenAI-compatible chat-completions endpoint. Both return
		// the same ChatResponse shape, so everything below is shared.
		//
		// With Config.StreamThinking the same dispatch takes the streaming
		// variants, forwarding deltas to OnDelta. The accumulated
		// ChatResponse they return is equivalent to the buffered one, so
		// the rest of the loop (History, usage, tool calls) is untouched.
		var resp *ChatResponse
		if h.Config.StreamThinking {
			onDelta := func(d ChatDelta) {
				if h.OnDelta == nil {
					return
				}
				if d.ReasoningContent != "" {
					h.OnDelta("thinking", d.ReasoningContent)
				}
				if d.Content != "" {
					h.OnDelta("content", d.Content)
				}
			}
			if h.Config.Provider == "ollama" {
				streamResp, err := h.Client.OllamaChatStream(ctx, chatReq, h.Config.MaxContextTokens, onDelta)
				if err != nil {
					return "", fmt.Errorf("ollama chat stream: %w", err)
				}
				resp = streamResp
			} else {
				streamResp, err := h.Client.ChatCompletionStream(ctx, chatReq, onDelta)
				if err != nil {
					return "", fmt.Errorf("chat completion stream: %w", err)
				}
				resp = streamResp
			}
		} else if h.Config.Provider == "ollama" {
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
		// Transcript logging (roadmap 3.1): the assistant response and
		// its usage land right next to the LastUsage record.
		h.logResponse(assistantMsg, resp.Usage)

		// No tool calls -> the model is giving its final answer.
		if len(assistantMsg.ToolCalls) == 0 {
			return ExtractText(assistantMsg.Content), nil
		}

		// Otherwise, run the requested tools (concurrently where safe -
		// roadmap 2.1) and feed the results back into the conversation in
		// the original call order, then loop so the model can respond to them.
		h.runToolCalls(ctx, assistantMsg.ToolCalls)
	}

	// Cap hit (and the loop body actually ran): give the model one last
	// chance to summarize its work into a final answer. The request drops
	// the tool schemas (Tools: nil) and tells the model plainly to stop
	// calling tools, so the only way it can "answer" is text. If even
	// that response errors or still insists on tool calls, fall back to
	// the original max-iterations error. Skipped when MaxToolIterations
	// is 0: the loop body never ran, so there is nothing to summarize.
	if h.Config.MaxToolIterations > 0 {
		answer, err := h.forceFinalAnswer(ctx)
		if err == nil {
			return answer, nil
		}
		return "", fmt.Errorf("hit max tool iterations (%d) without a final answer (forced final answer: %v)",
			h.Config.MaxToolIterations, err)
	}
	return "", fmt.Errorf("hit max tool iterations (%d) without a final answer", h.Config.MaxToolIterations)
}

// forceFinalAnswer is the graceful exit at the MaxToolIterations cap:
// append the "stop calling tools" user message, send exactly one more
// request with Tools: nil (same provider/stream dispatch, same
// temperature), and treat a tool-call-free reply as the turn's final
// answer - appended to History and updating LastUsage just like the
// normal path. Any request failure, or a reply that still carries tool
// calls, becomes an error the caller folds into the max-iterations error.
func (h *Harness) forceFinalAnswer(ctx context.Context) (string, error) {
	h.History = append(h.History, Message{
		Role:    "user",
		Content: "You have reached the tool-call limit. Respond with your best final answer now, without calling any tools.",
	})

	chatReq := ChatRequest{
		Model:       h.Config.Model,
		Messages:    h.History,
		Tools:       nil, // no schemas: the model can only answer in text
		Temperature: h.Config.Temperature,
		Stream:      false,
	}
	h.logRequest(chatReq)

	var resp *ChatResponse
	if h.Config.StreamThinking {
		onDelta := func(d ChatDelta) {
			if h.OnDelta == nil {
				return
			}
			if d.ReasoningContent != "" {
				h.OnDelta("thinking", d.ReasoningContent)
			}
			if d.Content != "" {
				h.OnDelta("content", d.Content)
			}
		}
		if h.Config.Provider == "ollama" {
			streamResp, err := h.Client.OllamaChatStream(ctx, chatReq, h.Config.MaxContextTokens, onDelta)
			if err != nil {
				return "", fmt.Errorf("ollama chat stream: %w", err)
			}
			resp = streamResp
		} else {
			streamResp, err := h.Client.ChatCompletionStream(ctx, chatReq, onDelta)
			if err != nil {
				return "", fmt.Errorf("chat completion stream: %w", err)
			}
			resp = streamResp
		}
	} else if h.Config.Provider == "ollama" {
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
	if len(assistantMsg.ToolCalls) > 0 {
		// The model ignored the no-tools instruction. Leave the message in
		// History (it's honest transcript) but let the caller report the
		// cap error rather than a dangling tool_calls turn.
		h.History = append(h.History, assistantMsg)
		return "", fmt.Errorf("model still requested %d tool call(s)", len(assistantMsg.ToolCalls))
	}

	h.History = append(h.History, assistantMsg)
	h.LastUsage = resp.Usage
	h.logResponse(assistantMsg, resp.Usage)
	return ExtractText(assistantMsg.Content), nil
}

// defaultMaxParallelTools is the fallback concurrency cap when
// Config.MaxParallelTools is zero or negative.
const defaultMaxParallelTools = 4

// runToolCalls dispatches one assistant turn's tool calls (roadmap 2.1)
// and appends one "tool" result message per call to History, in the
// ORIGINAL call order, exactly as the old sequential loop did -
// providers are order-sensitive, so the resulting History is
// byte-for-byte identical to sequential dispatch regardless of which
// calls actually ran concurrently.
//
// Scheduling scheme:
//   - Calls are partitioned in order into two classes. A call is SERIAL
//     when its tool implements SerialTyper with Serial() == true, when
//     the tool name is unknown to the Registry (the error path is cheap
//     and keeping unknowns out of the pool keeps classification simple),
//     or when its policy is PolicyAsk. Everything else is
//     concurrency-safe.
//   - The concurrency-safe calls are launched into a bounded worker pool
//     (a semaphore channel sized Config.MaxParallelTools, default
//     defaultMaxParallelTools); each goroutine calls h.runTool and
//     stores its result at the call's original index.
//   - Serial calls then execute one at a time in call order, while the
//     pool is still running, and the loop waits for the pool before
//     touching History.
//
// Ordering semantics: History order always equals call order, and serial
// calls keep their relative execution order, but a serial call may
// START before an earlier concurrency-safe call finishes (the pool runs
// in parallel with the serial pass). Cross-class side-effect ordering is
// therefore not preserved - acceptable per the roadmap design, since
// each tool still sees exactly its own arguments and side-effecting
// tools are all in the serial class anyway.
//
// The PolicyAsk-forces-serial rule exists because Harness.Confirm prompts
// on stdin in the REPL: two tools at PolicyAsk must NEVER prompt
// concurrently, so even a tool that forgot to mark itself serial is
// scheduled serially once it needs confirmation.
//
// Cancellation: tool.Execute takes no context (the Tool interface is
// unchanged by design), so a cancelled turn cannot abort an in-flight
// Execute. Instead, if ctx is already done, no call is launched and
// every un-run call gets a "tool call cancelled" result message, keeping
// the assistant tool_calls turn properly paired in History.
func (h *Harness) runToolCalls(ctx context.Context, calls []ToolCall) {
	// Partition in call order.
	type pending struct {
		idx  int
		call ToolCall
	}
	var safe, serial []pending
	for i, call := range calls {
		isSerial := false
		if tool, found := h.Registry.Get(call.Function.Name); !found {
			isSerial = true // unknown tool: cheap error path, keep it out of the pool
		} else if s, ok := tool.(SerialTyper); ok && s.Serial() {
			isSerial = true
		} else if h.Registry.PolicyFor(call.Function.Name) == PolicyAsk {
			isSerial = true // confirmation reads stdin: never prompt two tools at once
		}
		if isSerial {
			serial = append(serial, pending{i, call})
		} else {
			safe = append(safe, pending{i, call})
		}
	}

	results := make([]string, len(calls))

	if ctx.Err() != nil {
		// Turn cancelled before dispatch: keep History valid by pairing
		// every tool_call with a cancellation result, running nothing.
		for i := range results {
			results[i] = "tool call cancelled"
		}
	} else {
		max := h.Config.MaxParallelTools
		if max <= 0 {
			max = defaultMaxParallelTools
		}
		sem := make(chan struct{}, max)

		var wg sync.WaitGroup
		for _, p := range safe {
			wg.Add(1)
			go func(p pending) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				// runTool is safe to call from multiple goroutines for
				// concurrency-safe tools: the Registry is only read here
				// and History is written solely in the main goroutine.
				results[p.idx] = h.runTool(p.call)
			}(p)
		}

		// Serial pass runs while the pool is still in flight, one call at
		// a time in call order. If the turn is cancelled mid-pass, the
		// remaining serial calls are not run (in-flight pool Execute
		// calls can't be aborted - the Tool interface has no ctx).
		for _, p := range serial {
			if ctx.Err() != nil {
				results[p.idx] = "tool call cancelled"
				continue
			}
			results[p.idx] = h.runTool(p.call)
		}
		wg.Wait()
	}

	// Append in the ORIGINAL call order, exactly like the old loop.
	for i, call := range calls {
		h.History = append(h.History, Message{
			Role:       "tool",
			Content:    results[i],
			ToolCallID: call.ID,
			Name:       call.Function.Name,
		})
	}
}

// runTool dispatches a single tool call. Errors are stringified rather
// than returned to the caller, because the intended recipient of an error
// here is the MODEL (so it can see what went wrong and try again), not the
// Go caller of Ask().
//
// The Registry's permission policy (roadmap 1.2) is enforced here too:
// denied tools and calls the user declined via the Harness.Confirm hook
// both come back as model-visible strings instead of running the tool.
func (h *Harness) runTool(call ToolCall) string {
	tool, ok := h.Registry.Get(call.Function.Name)
	if !ok {
		return fmt.Sprintf("error: no such tool %q", call.Function.Name)
	}

	// Permission gate (roadmap 1.2), before Execute and before output
	// truncation: the gate's own strings are short and stay whole.
	switch h.Registry.PolicyFor(call.Function.Name) {
	case PolicyDeny:
		return fmt.Sprintf("error: tool %q is not permitted by configuration", call.Function.Name)
	case PolicyAsk:
		if h.Confirm == nil {
			h.warnAskWithoutConfirm()
		} else if !h.Confirm(call) {
			return fmt.Sprintf("user declined to run tool %q", call.Function.Name)
		}
	}

	result, err := tool.Execute(json.RawMessage(call.Function.Arguments))
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	// Cap successful results only: error strings are left untouched so
	// the model always sees the full failure. A tool implementing
	// MaxOutputTyper overrides the global cap for itself (0 = unlimited).
	limit := h.Config.MaxToolOutputBytes
	if typer, ok := tool.(MaxOutputTyper); ok {
		limit = typer.MaxOutputBytes()
	}
	result = truncateToolOutput(result, limit)
	// Transcript logging (roadmap 3.1): name, arguments and the
	// truncated result, exactly what the model sees fed back.
	h.logToolCall(call, result)
	return result
}

// askNoConfirmWarned makes the "PolicyAsk but no Confirm hook" warning
// print once per process, so an embedder that configures ask policies
// without wiring a prompter learns about it once instead of per call.
var askNoConfirmWarned bool

func (h *Harness) warnAskWithoutConfirm() {
	if askNoConfirmWarned {
		return
	}
	askNoConfirmWarned = true
	fmt.Fprintln(os.Stderr, "warning: tool policy is ask but Harness.Confirm is not set; auto-allowing")
}

// truncateToolOutput cuts a tool result longer than capBytes down to a
// head plus tail with a "[truncated N of M bytes]" marker in between.
// capBytes counts bytes and budgets head+tail (capBytes/2 of head, the
// rest as tail); the marker is fixed overhead on top, so the result lands
// at roughly capBytes plus the marker's length.
//
// Cuts snap to UTF-8 rune boundaries (never splitting a multi-byte
// character): a rune straddling the head cut is dropped whole (head snaps
// back to the last rune start), and the tail resumes at the first rune
// start at or after its raw cut point. capBytes <= 0 means unlimited: the
// result is returned byte-for-byte.
func truncateToolOutput(result string, capBytes int) string {
	if capBytes <= 0 || len(result) <= capBytes {
		return result
	}

	// Head: snap the raw capBytes/2 index back to a rune boundary.
	headCut := capBytes / 2
	for headCut > 0 && !utf8.RuneStart(result[headCut]) {
		headCut--
	}

	// Tail: take the last capBytes-capBytes/2 bytes, snapped forward so
	// the tail begins on a whole rune.
	tailCut := len(result) - (capBytes - capBytes/2)
	for tailCut < len(result) && !utf8.RuneStart(result[tailCut]) {
		tailCut++
	}

	kept := headCut + (len(result) - tailCut)
	marker := fmt.Sprintf("...[truncated %d of %d bytes - narrow your request]...",
		len(result)-kept, len(result))
	return result[:headCut] + marker + result[tailCut:]
}
