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
	"ai-harness/harness"
	"ai-harness/harness/config"
	luatool "ai-harness/harness/lua-tool"
	"ai-harness/harness/tools"
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	cfg := harness.TestConfig()
	conf := config.GetConfig()

	// --- Edit these to point at a different model/server -------------
	// cfg.Model = "qwen2.5:14b"
	// cfg.BaseURL = "http://localhost:8080/v1" // e.g. llama.cpp server
	// cfg.Temperature = 0.2
	// -------------------------------------------------------------------

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
	registry.Register(tools.ListFilesTool{BaseDir: "."})
	registry.Register(tools.ReadPartialFileTool{BaseDir: "."})
	registry.Register(tools.GetFileSizeTool{BaseDir: "."})
	registry.Register(tools.EditFileTool{BaseDir: "."})
	registry.Register(tools.WriteFileTool{BaseDir: "."})

	// --- Dynamic Custom Lua Tools Loader -------------------------------
	defaultConfigDir := "agent/custom_tools"
	if conf.CustomToolDir != "" {
		defaultConfigDir = conf.CustomToolDir
	}
	customToolsDir := defaultConfigDir
	entries, err := os.ReadDir(customToolsDir)
	if err != nil {
		fmt.Printf("Note: Skipping custom Lua tools (could not read directory %q: %v)\n", customToolsDir, err)
	} else {
		for _, entry := range entries {
			// Skip subdirectories and non-Lua files
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".lua" {
				continue
			}

			scriptPath := filepath.Join(customToolsDir, entry.Name())
			data, err := os.ReadFile(scriptPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to read custom tool %s: %v\n", scriptPath, err)
				continue
			}

			tool, err := luatool.NewLuaToolFromScriptWithConfig(string(data), luatool.FilesystemConfig{BaseDir: "."})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to parse Lua tool %s: %v\n", scriptPath, err)
				continue
			}

			registry.Register(tool)
			fmt.Printf("Loaded custom Lua tool: %s\n", entry.Name())
		}
	}

	h := harness.New(cfg, client, registry)

	fmt.Printf("ai-harness ready - model %q at %s. Type 'exit' to quit.\n", cfg.Model, cfg.BaseURL)

	scanner := bufio.NewScanner(os.Stdin)
	if err := scanner.Err(); err != nil {
		panic(err)
	}
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

		// --- Hot Reload Check ---
		// Automatically picks up edits or newly created scripts before every prompt
		if conf.HotReload {
			reloadLuaTools(customToolsDir, registry)
		}

		reply, err := h.Ask(ctx, input)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			continue
		}
		fmt.Println(reply)
		switch {
		case h.LastUsage.TotalTokens == 0:
			// This is the "nothing printed" case - the server's response
			// simply didn't include a usage field. Not every
			// OpenAI-compatible server populates this for every request.
			fmt.Println("[context: this server didn't report token usage]")
		case cfg.MaxContextTokens > 0:
			pct := 100 * h.LastUsage.PromptTokens / cfg.MaxContextTokens
			fmt.Printf("[context: %d / %d tokens (%d%%)]\n",
				h.LastUsage.PromptTokens, cfg.MaxContextTokens, pct)
			if pct >= 80 {
				fmt.Println("[warning: approaching context limit - consider trimming history]")
			}
		default:
			fmt.Printf("[context: %d prompt + %d completion = %d tokens]\n",
				h.LastUsage.PromptTokens, h.LastUsage.CompletionTokens, h.LastUsage.TotalTokens)
		}
	}
}

var luaModTimes = make(map[string]time.Time)

func reloadLuaTools(dir string, registry *harness.Registry) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".lua" {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		lastMod, known := luaModTimes[path]
		// If new file OR modified since last check
		if !known || info.ModTime().After(lastMod) {
			data, err := os.ReadFile(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[hot-reload error reading %s]: %v\n", entry.Name(), err)
				continue
			}

			tool, err := luatool.NewLuaToolFromScriptWithConfig(string(data), luatool.FilesystemConfig{BaseDir: "."})
			if err != nil {
				fmt.Fprintf(os.Stderr, "[hot-reload syntax error in %s]: %v\n", entry.Name(), err)
				continue
			}

			registry.Register(tool)
			luaModTimes[path] = info.ModTime()
		}
	}
}
