package main

import (
	"ai-harness/bdo/bdotools"
	"ai-harness/harness"
	"ai-harness/harness/config"
	luatool "ai-harness/harness/lua-tool"
	"ai-harness/harness/mcp"
	"ai-harness/harness/tools"
	"ai-harness/harness/workflow"
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
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

	cfg := harness.StrataConfig()
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
	registry.Register(tools.ShellTool{BaseDir: "."})
	// Web tools (roadmap 2.3) are read-only, so the default PolicyAllow
	// applies; config's tool-permissions lists can still gate them via
	// applyToolPolicies below.
	registry.Register(tools.WebFetchTool{})
	registry.Register(tools.WebSearchTool{})
	bdotools.RegisterMarketTools(registry)

	// MCP tool servers (roadmap 2.2): spawn each configured server over
	// stdio and register its discovered tools. A failing server is a
	// warning, never a startup abort. The MCP tools keep the Registry's
	// zero-value PolicyAllow, but that is the same default applyToolPolicies
	// below encodes for unlisted names, and any explicit
	// tool-permissions entry for an MCP tool name still wins, because
	// ApplyPolicies is keyed purely by name.
	var mcpClients []*mcp.Client
	defer func() {
		for _, c := range mcpClients {
			_ = c.Close() // also terminates the server process
		}
	}()
	// Bounded startup so an MCP server that never answers tools/list
	// can't hang the REPL; the registered tools use their own context
	// at call time, so this one is cancelled once startup finishes.
	ctx0, cancelMCPStartup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelMCPStartup()
	for _, srv := range conf.MCPServers {
		client, _, err := mcp.StartServer(srv.Command, srv.Args, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to start MCP server %q (%s): %v\n", srv.Name, srv.Command, err)
			continue
		}
		mcpClients = append(mcpClients, client)
		n, err := mcp.LoadTools(ctx0, client, registry, srv.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load tools from MCP server %q: %v\n", srv.Name, err)
			continue
		}
		fmt.Printf("Loaded %d MCP tools from server %q\n", n, srv.Name)
	}

	// Permission policies (roadmap 1.2): config.yaml's tool-permissions
	// lists plus a default ask for run_shell_command. applyToolPolicies
	// runs again on hot-reload below, so config edits take effect
	// without restarting the REPL.
	applyToolPolicies(conf, registry)

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

	// Persistent memory (roadmap 2.4): config.yaml's memory-file picks
	// the store; an EMPTY value still defaults to agent/memory.md for
	// the REPL. Workflows never set harness Config.Memory, so they stay
	// memory-free. The remember tool is registered at the Registry's
	// zero-value PolicyAllow; any explicit tool-permissions entry for
	// "remember" still wins, because ApplyPolicies is keyed by name.
	memFile := conf.MemoryFile
	if memFile == "" {
		memFile = "agent/memory.md"
	}
	cfg.Memory = &harness.Memory{Path: memFile}

	h := harness.New(cfg, client, registry)
	registry.Register(harness.RememberTool{H: h})

	// The REPL's stdin scanner, created before h.Confirm below so the
	// confirmation prompt can share it (Confirm and the prompt loop
	// never read concurrently: Confirm only runs from inside h.Ask,
	// which runs inside the loop body).
	scanner := bufio.NewScanner(os.Stdin)
	if err := scanner.Err(); err != nil {
		panic(err)
	}

	// Confirm gates PolicyAsk tools in the terminal (roadmap 1.2). It
	// reads from the same stdin scanner as the REPL loop. EOF = decline.
	h.Confirm = func(call harness.ToolCall) bool {
		fmt.Printf("\n[confirm] run tool %q with args: %s\n[y/N] ", call.Function.Name, call.Function.Arguments)
		if !scanner.Scan() {
			return false
		}
		answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
		return answer == "y" || answer == "yes"
	}

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
	fmt.Printf("Persistent memory: %s\n", memFile)

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
			// viper's OnConfigChange repopulates conf in place, so
			// re-applying here makes tool-permissions edits live too.
			applyToolPolicies(conf, registry)
		}

		// Slash commands (roadmap 1.3): any line starting with '/' is
		// handled locally and never reaches the model. Handled after the
		// hot-reload block so /tools reflects freshly reloaded
		// tools/policies, and before the image-token extraction below.
		if handleCommand(input, h, &cfg, registry) {
			continue
		}

		// Tokens that start with '@' (e.g. @C:\pics\cat.png or @./img/shot.jpg)
		// are pulled out of the line as image file paths and sent through
		// AskWithImages, which loads them itself and reports per-file errors
		// to stderr while continuing the prompt.
		cleanedInput, imagePaths := extractImageTokens(input)

		// Ctrl+C cancellation (roadmap 3.3): the turn runs under a
		// context that SIGINT cancels, so an interrupt aborts the
		// in-flight model/tool HTTP requests instead of killing the
		// process, and askLoop rolls History back to its pre-Ask
		// state - the session stays usable at the next prompt.
		var reply string
		interrupted, askErr := runWithInterrupt(func(ctx context.Context) error {
			var aerr error
			if len(imagePaths) > 0 {
				reply, aerr = h.AskWithImages(ctx, cleanedInput, imagePaths)
			} else {
				reply, aerr = h.Ask(ctx, cleanedInput)
			}
			return aerr
		})
		if interrupted {
			continue
		}
		if askErr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", askErr)
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

// handleCommand implements the REPL slash commands (roadmap 1.3, plus
// /compact from 1.1 and /memory from 2.4): /help (and bare /), /reset,
// /save, /load, /history, /tools, /model, /compact and /memory. Any line starting with '/' is consumed
// here - unknown commands print "unknown command: /foo" plus a one-line
// usage list - and the return value reports whether the input WAS a
// command (true means: do not send it to the model). No slash command
// reads stdin, so this never races with the h.Confirm prompt that shares
// the REPL scanner. /compact's summary request is the one command that
// talks to the server, so it runs under runWithInterrupt (roadmap 3.3):
// Ctrl+C cancels it mid-request and History stays untouched (Compact
// only rewrites it after the summary call fully succeeds).
//
// /model sets h.Config.Model, which is the field askLoop reads on every
// request; cfg (main's pre-New copy, only used for startup) is kept in
// sync so it never goes stale.
func handleCommand(input string, h *harness.Harness, cfg *harness.Config, registry *harness.Registry) bool {
	if !strings.HasPrefix(input, "/") {
		return false
	}

	fields := strings.Fields(input)
	name, arg := fields[0], strings.TrimSpace(input[len(fields[0]):])

	switch strings.ToLower(name) {
	case "/", "/help":
		printCommands()
	case "/reset":
		h.Reset()
		fmt.Println("started a new conversation (config and tools kept)")
	case "/save":
		if arg == "" {
			fmt.Println("usage: /save <file>")
			return true
		}
		if err := h.SaveSession(arg); err != nil {
			fmt.Fprintf(os.Stderr, "save failed: %v\n", err)
			return true
		}
		fmt.Printf("saved %d messages to %s\n", len(h.History), arg)
	case "/load":
		if arg == "" {
			fmt.Println("usage: /load <file>")
			return true
		}
		if err := h.LoadSession(arg); err != nil {
			fmt.Fprintf(os.Stderr, "load failed: %v\n", err)
			return true
		}
		fmt.Printf("loaded %d messages (model %s)\n", len(h.History), h.Config.Model)
	case "/history":
		for i, m := range h.History {
			line := fmt.Sprintf("%3d  %-9s %s", i, m.Role, previewText(harness.ExtractText(m.Content), 120))
			if n := len(m.ToolCalls); n > 0 {
				line += fmt.Sprintf(" [+%d tool calls]", n)
			}
			if n := len(m.Images); n > 0 {
				line += fmt.Sprintf(" [+%d images]", n)
			}
			fmt.Println(line)
		}
	case "/tools":
		for _, p := range registry.Policies() {
			fmt.Printf("%s  [%s]\n", p.Name, policyName(p.Policy))
		}
	case "/model":
		if arg == "" {
			fmt.Printf("current model: %s\n", h.Config.Model)
			return true
		}
		cfg.Model = arg
		h.Config.Model = arg
		fmt.Printf("model set to %s\n", arg)
	case "/memory":
		// Persistent memory (roadmap 2.4): path, entry count, and the
		// most recent entries (newest last). Read errors are reported,
		// not fatal.
		if h.Memory == nil {
			fmt.Println("memory is disabled (set memory-file in config.yaml)")
			return true
		}
		entries, err := h.Memory.Load()
		if err != nil {
			fmt.Fprintf(os.Stderr, "memory read failed: %v\n", err)
			return true
		}
		fmt.Printf("memory file: %s (%d entries)\n", h.Memory.Path, len(entries))
		const show = 10
		if len(entries) > show {
			fmt.Printf("... (%d older entries omitted)\n", len(entries)-show)
			entries = entries[len(entries)-show:]
		}
		for _, e := range entries {
			fmt.Println(e)
		}
	case "/compact":
		// Manual compaction (roadmap 1.1): summarizes the older History
		// into one system message. Compact ignores Config.CompactThreshold,
		// so this works even when the automatic trigger is disabled; a
		// failed or interrupted summary leaves History untouched (the
		// summary call only rewrites History after it fully succeeds).
		before := len(h.History)
		var changed bool
		interrupted, err := runWithInterrupt(func(ctx context.Context) error {
			var cerr error
			changed, cerr = h.Compact(ctx)
			return cerr
		})
		if interrupted {
			return true
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "compact failed: %v\n", err)
			return true
		}
		if !changed {
			fmt.Println("nothing to compact")
			return true
		}
		fmt.Printf("compacted history: %d -> %d messages\n", before, len(h.History))
	default:
		fmt.Printf("unknown command: %s\n", name)
		printCommandUsage()
	}
	return true
}

// runWithInterrupt (roadmap 3.3) runs fn under a context that Ctrl+C
// (os.Interrupt) cancels instead of killing the process: the in-flight
// model/tool HTTP requests abort, fn's error wraps context.Canceled,
// and interrupted reports true after printing the notice, so the REPL
// re-prompts with the session intact.
//
// Signal handling is installed for fn's duration only: Ctrl+C at an
// idle prompt keeps the original behavior (default handling exits the
// process). A second Ctrl+C during a turn force-quits, because
// signal.NotifyContext restores the default handler once it fires.
//
// Ctrl+C while a PolicyAsk confirmation is waiting on stdin lands as
// a cancelled turn: the prompt still consumes the typed line, then
// askLoop cancels the remaining calls and rolls History back.
func runWithInterrupt(fn func(ctx context.Context) error) (interrupted bool, err error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err = fn(ctx)
	if err != nil && ctx.Err() != nil && errors.Is(err, context.Canceled) {
		fmt.Println("[interrupted - session preserved]")
		return true, nil
	}
	return false, err
}

// printCommandUsage is the one-line command summary printed after an
// unknown slash command; printCommands is the /help list with
// per-command descriptions.
func printCommandUsage() {
	fmt.Println("commands: /help  /reset  /save <file>  /load <file>  /history  /tools  /model [name]  /compact  /memory")
}

func printCommands() {
	fmt.Println("  /help or /      show this help")
	fmt.Println("  /reset          start a new conversation (keeps config and tools)")
	fmt.Println("  /save <file>    save the conversation as JSON (atomic write)")
	fmt.Println("  /load <file>    load a saved conversation (also restores its model)")
	fmt.Println("  /history        list messages with truncated one-line previews")
	fmt.Println("  /tools          list registered tools with their permission policy")
	fmt.Println("  /model <name>   switch the model live (bare /model prints the current one)")
	fmt.Println("  /compact        summarize the oldest history into one system message")
	fmt.Println("  /memory         show the persistent memory file, entry count, and recent entries")
	fmt.Println("  Ctrl+C          cancel an in-flight request (session preserved); press again to force-quit")
}

// policyName renders a Registry permission policy for /tools output.
func policyName(p harness.Policy) string {
	switch p {
	case harness.PolicyAsk:
		return "ask"
	case harness.PolicyDeny:
		return "deny"
	default:
		return "allow"
	}
}

// previewText flattens newlines (a /history line must never contain raw
// line breaks) and truncates to max runes with an ellipsis.
func previewText(s string, max int) string {
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
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

// applyToolPolicies pushes config.yaml's tool-permissions lists onto
// the registry (roadmap 1.2), with run_shell_command defaulting to ask
// unless config explicitly lists it under allow or deny. ApplyPolicies
// replaces the whole mapping, so names removed from config revert to
// allow on the next reload.
func applyToolPolicies(conf *config.Config, registry *harness.Registry) {
	allow, ask, deny := conf.ToolPermissions.Allow, conf.ToolPermissions.Ask, conf.ToolPermissions.Deny
	explicit := false
	for _, n := range append(append([]string{}, allow...), deny...) {
		if n == "run_shell_command" {
			explicit = true
			break
		}
	}
	if !explicit {
		ask = append(ask, "run_shell_command")
	}
	registry.ApplyPolicies(allow, ask, deny)
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
