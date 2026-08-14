// ai-harness is a minimal, hackable harness for talking to a local LLM
// server (Ollama, llama.cpp, LM Studio, etc.) with tool-calling support.
//
// The design is intentionally simple and split into small pieces so you
// can edit any one part without needing to understand the whole thing:
//
//	harness/types.go   - the wire format (messages, tool schemas)
//	harness/config.go  - settings: model, server URL, prompt, limits
//	harness/client.go  - HTTP call to the model server
//	harness/tools.go   - Tool interface + example tools
//	harness/harness.go - the agent loop that ties it all together
//	main.go            - this file: wires everything up and runs a REPL
//
// To point this at a real model server: run Ollama (or similar) locally,
// pull a tool-capable model (e.g. `ollama pull llama3.1`), then
// `go run .` and start chatting.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"ai-harness/harness"
	"ai-harness/harness/tools"
)

func main() {
	cfg := harness.DefaultConfig()

	// --- Edit these to point at a different model/server -------------
	// cfg.Model = "qwen2.5:14b"
	// cfg.BaseURL = "http://localhost:8080/v1" // e.g. llama.cpp server
	// cfg.Temperature = 0.2
	// -------------------------------------------------------------------

	// Read the API key from an environment variable rather than hardcoding
	// it - keeps secrets out of source control. Set it before running, e.g.:
	//   export AI_HARNESS_API_KEY=sk-...
	// Leave it unset for local servers that don't require auth.
	cfg.APIKey = "sk-unsloth-94d524b13d50f995c58cbb5d08c090c3" //os.Getenv("AI_HARNESS_API_KEY")
	cfg.BaseURL = "http://192.168.1.235:8810/v1"
	cfg.Model = "qwen3-coder-next-gguf"

	client := harness.NewClient(cfg.BaseURL, cfg.APIKey)

	// Ask the server for this model's real context window rather than
	// guessing. This only works against Ollama's native /api/show
	// endpoint - if it fails (different server, model not pulled yet,
	// etc.) we just skip the percentage display below.
	ctxProbe, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if n, err := client.ContextLength(ctxProbe, cfg.Model); err == nil {
		cfg.MaxContextTokens = n
	}
	cancel()

	// Register whatever tools you want the model to have access to.
	// See harness/tools.go for the Tool interface and more examples.
	registry := harness.NewRegistry()
	registry.Register(tools.CurrentTimeTool{})
	registry.Register(tools.ReadFileTool{BaseDir: "."})
	// registry.Register(mytools.WebSearchTool{APIKey: os.Getenv("SEARCH_KEY")})

	h := harness.New(cfg, client, registry)

	fmt.Printf("ai-harness ready - model %q at %s. Type 'exit' to quit.\n", cfg.Model, cfg.BaseURL)

	scanner := bufio.NewScanner(os.Stdin)
	ctx := context.Background()

	for {
		fmt.Print("\n> ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			break
		}

		reply, err := h.Ask(ctx, input)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			continue
		}
		fmt.Println(reply)
		if h.LastUsage.TotalTokens > 0 {
			if cfg.MaxContextTokens > 0 {
				pct := 100 * h.LastUsage.PromptTokens / cfg.MaxContextTokens
				fmt.Println(pct)
				fmt.Printf("[context: %d / %d tokens (%d%%)]\n",
					h.LastUsage.PromptTokens, cfg.MaxContextTokens, pct)
				if pct >= 80 {
					fmt.Println("[warning: approaching context limit - consider trimming history]")
				}
			} else {
				fmt.Printf("[context: %d prompt + %d completion = %d tokens]\n",
					h.LastUsage.PromptTokens, h.LastUsage.CompletionTokens, h.LastUsage.TotalTokens)
			}
		}
	}
}
