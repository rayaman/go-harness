package main

import (
	"ai-harness/bdo/bdotools"
	"ai-harness/harness"
	"ai-harness/harness/config"
	luatool "ai-harness/harness/lua-tool"
	"ai-harness/harness/tools"
	"ai-harness/harness/workflow"
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	// --- Workflow mode -------------------------------------------------
	// go run main.go --workflow path/to/workflow [extra args after --]
	// Runs a declarative directory workflow (workflow.yaml / main.lua)
	// against its own config overrides, then exits. The REPL below is
	// skipped entirely. See plans/workflows.md and docs/workflows.md.
	workflowDir := flag.String("workflow", "", "path to a workflow directory to run instead of the REPL")
	basePreset := flag.String("base-config", "ollama", `base config for workflows: "ollama" or "openai"`)
	streamThinking := flag.Bool("stream-thinking", false, "stream model thinking/reasoning output live")
	flag.Parse()

	if *workflowDir != "" {
		if err := runWorkflow(*workflowDir, *basePreset, *streamThinking, flag.Args()); err != nil {
			fmt.Fprintf(os.Stderr, "workflow error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	// ---------------------------------------------------------------------

	cfg := harness.OllamaConfig()
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
	bdotools.RegisterMarketTools(registry)

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

	cfg.StreamThinking = *streamThinking

	h := harness.New(cfg, client, registry)

	// With --stream-thinking, print deltas live: thinking traces on
	// their own "[thinking] "-prefixed lines, content as it arrives.
	// streamedContent tracks whether the reply text already went out, so
	// the final print below doesn't duplicate it.
	streamedContent := false
	if cfg.StreamThinking {
		thinkBuf := ""
		h.OnDelta = func(kind, text string) {
			switch kind {
			case "thinking":
				thinkBuf += text
				for {
					i := strings.IndexByte(thinkBuf, '\n')
					if i < 0 {
						break
					}
					fmt.Printf("[thinking] %s\n", thinkBuf[:i])
					thinkBuf = thinkBuf[i+1:]
				}
			case "content":
				if thinkBuf != "" {
					fmt.Printf("[thinking] %s\n", thinkBuf)
					thinkBuf = ""
				}
				streamedContent = true
				fmt.Print(text)
			}
		}
	}

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

		// Tokens that start with '@' (e.g. @C:\pics\cat.png or @./img/shot.jpg)
		// are pulled out of the line as image file paths and sent through
		// AskWithImages, which loads them itself and reports per-file errors
		// to stderr while continuing the prompt.
		cleanedInput, imagePaths := extractImageTokens(input)

		var reply string
		if len(imagePaths) > 0 {
			reply, err = h.AskWithImages(ctx, cleanedInput, imagePaths)
		} else {
			reply, err = h.Ask(ctx, cleanedInput)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			continue
		}
		if streamedContent {
			// The reply already streamed live; just close the line.
			fmt.Println()
		} else {
			fmt.Println(reply)
		}
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

// extractImageTokens splits a REPL input line on whitespace and pulls out
// every token that begins with '@' followed by at least one character. The
// '@' (and any single pair of matching surrounding quotes, e.g.
// `@"C:\pics\cat.png"`) is stripped and the remainder is collected as an
// image file path; the rest of the line is rejoined as the model prompt.
// A bare '@' is kept as literal text. The paths are NOT read or encoded
// here - Harness.AskWithImages does that and warns per-file on failure.
func extractImageTokens(input string) (string, []string) {
	var kept, imagePaths []string
	for _, tok := range strings.Fields(input) {
		if tok == "@" || tok[0] != '@' {
			kept = append(kept, tok)
			continue
		}
		path := tok[1:]
		if len(path) >= 2 && (path[0] == '"' || path[0] == '\'') && path[len(path)-1] == path[0] {
			path = path[1 : len(path)-1]
		}
		if path == "" {
			kept = append(kept, tok)
			continue
		}
		imagePaths = append(imagePaths, path)
	}
	return strings.Join(kept, " "), imagePaths
}

// runWorkflow loads a workflow directory and executes it against the
// selected base config preset. args become harness.args for main.lua
// workflows (everything after the workflow directory on the command line).
// streamThinking is the base default for Config.StreamThinking; a
// workflow's config.stream_thinking still overrides it in MergeConfig.
func runWorkflow(dir, basePreset string, streamThinking bool, args []string) error {
	var base harness.Config
	switch strings.ToLower(basePreset) {
	case "ollama":
		base = harness.OllamaConfig()
	case "openai", "default":
		base = harness.DefaultConfig()
	default:
		return fmt.Errorf("unknown --base-config %q (use \"ollama\" or \"openai\")", basePreset)
	}
	base.StreamThinking = streamThinking

	wf, err := workflow.Load(dir)
	if err != nil {
		return err
	}
	runner, err := workflow.New(wf, base, os.Stdout)
	if err != nil {
		return err
	}
	defer runner.Close()

	return runner.Run(context.Background(), args)
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
