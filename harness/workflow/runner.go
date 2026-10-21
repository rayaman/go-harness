package workflow

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"ai-harness/harness"
	"ai-harness/harness/config"
	luatool "ai-harness/harness/lua-tool"
	"ai-harness/harness/mcp"
	"ai-harness/harness/tools"
	"ai-harness/harness/utils"
)

// Runner executes a loaded Workflow: it assembles the Config/Client/Registry,
// then either runs the declarative steps loop or a main.lua entry point.
//
// One "active Harness" carries conversation history across steps and across
// harness.ask() calls from Lua; a step (or opts) that sets system_prompt -
// or opts.reset - starts a fresh conversation instead.
type Runner struct {
	wf     *Workflow
	cfg    harness.Config // workflow config merged onto the base
	client *harness.Client

	// tools holds every tool the workflow loaded (builtin + Lua + MCP),
	// keyed by name. Per-step `tools:` filters select a subset from
	// this map.
	tools    map[string]harness.Tool
	luaTools []*luatool.LuaTool // kept so Close can release their VMs

	// mcpClients holds the stdio clients for MCP servers declared in
	// config.yaml (roadmap 2.2); kept so Close can terminate the
	// server processes. Their discovered tools live in the tools map
	// like any other, so `tools: builtin: [...]`/`all` and per-step
	// selectors can name them.
	mcpClients []*mcp.Client

	out io.Writer

	// stepOutputs maps step names (and save_as aliases) to their final
	// text output, for {{steps.name}} templating and step_results().
	stepOutputs map[string]string

	// stepJSON holds the decoded JSON value of schema-validated steps
	// (roadmap 2.5), keyed the same way as stepOutputs (name + save_as
	// aliases), for {{steps.name.field}} templating and step_json().
	stepJSON map[string]any

	sess *session

	// perm{Allow,Ask,Deny} are the tool-permissions lists from
	// config.yaml (roadmap 1.2), applied to every per-step registry
	// built by registryFor (deny beats ask beats allow on collision).
	permAllow []string
	permAsk   []string
	permDeny  []string

	// thinking buffers streamed thinking deltas (when cfg.StreamThinking)
	// into "[thinking] "-prefixed lines on out. Nil when streaming is off.
	thinking *thinkingPrinter
}

// thinkingPrinter turns raw thinking deltas into "[thinking] "-prefixed
// lines on a writer: deltas arrive as arbitrary token-sized fragments, so
// partial lines are buffered until a newline arrives or flush() closes out
// the thinking phase.
type thinkingPrinter struct {
	out io.Writer
	buf []byte
}

func (t *thinkingPrinter) writeThinking(text string) {
	t.buf = append(t.buf, text...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		fmt.Fprintf(t.out, "[thinking] %s\n", t.buf[:i])
		t.buf = t.buf[i+1:]
	}
}

func (t *thinkingPrinter) flush() {
	if len(t.buf) > 0 {
		fmt.Fprintf(t.out, "[thinking] %s\n", t.buf)
		t.buf = nil
	}
}

// session is the mutable execution state shared by the steps loop and the
// Lua harness module: the active Harness (nil until first use, so
// conversation history carries across steps) and CLI args for main.lua.
type session struct {
	r    *Runner
	h    *harness.Harness
	args []string
}

// New loads the workflow's tools and prepares a Runner. It probes the model
// server for the context window when the config leaves MaxContextTokens at 0.
// out receives progress lines and step outputs (nil = os.Stdout).
func New(wf *Workflow, base harness.Config, out io.Writer) (*Runner, error) {
	if out == nil {
		out = os.Stdout
	}
	cfg, err := wf.MergeConfig(base)
	if err != nil {
		return nil, err
	}
	r := &Runner{
		wf:          wf,
		cfg:         cfg,
		out:         out,
		tools:       map[string]harness.Tool{},
		stepOutputs: map[string]string{},
		stepJSON:    map[string]any{},
	}
	r.client = harness.NewClient(r.cfg.BaseURL, r.cfg.APIKey)
	// Zero RequestTimeout is a no-op here, leaving NewClient's 5m default.
	r.client.SetRequestTimeout(r.cfg.RequestTimeout)
	// MaxRetries <= 0 likewise leaves the client's no-retry default; the
	// presets ship 2 (roadmap 1.4).
	r.client.SetMaxRetries(r.cfg.MaxRetries)

	// Same auto-probe the REPL does in main.go: trust the server over
	// the config when it reports a context window.
	if r.cfg.MaxContextTokens == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if n, err := r.client.ContextLength(ctx, r.cfg.Model); err == nil {
			r.cfg.MaxContextTokens = n
		}
	}

	if err := r.loadTools(); err != nil {
		return nil, err
	}
	if conf := config.GetConfig(); conf != nil {
		r.permAllow = conf.ToolPermissions.Allow
		r.permAsk = conf.ToolPermissions.Ask
		r.permDeny = conf.ToolPermissions.Deny
	}
	r.sess = &session{r: r}
	return r, nil
}

// Close releases any Lua tool VMs and MCP server clients held by the
// runner (closing an MCP client terminates its server process).
func (r *Runner) Close() {
	for _, t := range r.luaTools {
		t.Close()
	}
	for _, c := range r.mcpClients {
		_ = c.Close()
	}
}

// newHarness builds a Harness for the runner's config and, when
// StreamThinking is enabled, wires OnDelta so thinking deltas print live
// to r.out as "[thinking] ..." lines. Content deltas are deliberately
// ignored here: workflow step output is captured and printed once at the
// end of the step (see runStep), so streaming content live would
// double-print it.
func (r *Runner) newHarness(cfg harness.Config, reg *harness.Registry) *harness.Harness {
	h := harness.New(cfg, r.client, reg)
	// Permission prompt (roadmap 1.2): workflows are non-interactive by
	// default, so Confirm stays nil (auto-allow) unless workflow.yaml
	// sets permissions.ask. When set, the prompt prints on r.out and
	// reads y/N from stdin; EOF = decline.
	if r.wf.Permissions.Ask {
		h.Confirm = func(call harness.ToolCall) bool {
			fmt.Fprintf(r.out, "\n[confirm] run tool %q with args: %s\n[y/N] ",
				call.Function.Name, call.Function.Arguments)
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil && line == "" {
				return false
			}
			answer := strings.ToLower(strings.TrimSpace(line))
			return answer == "y" || answer == "yes"
		}
	}
	if r.cfg.StreamThinking {
		tp := &thinkingPrinter{out: r.out}
		r.thinking = tp
		h.OnDelta = func(kind, text string) {
			switch kind {
			case "thinking":
				tp.writeThinking(text)
			case "content":
				// Thinking phase is over: complete any partial line.
				tp.flush()
			}
		}
	}
	return h
}

// -----------------------------------------------------------------------------
// Tool loading
// -----------------------------------------------------------------------------

// builtinTools maps the names allowed in `tools.builtin` to constructors.
// BaseDir for file tools is the workflow's WorkDir.
func builtinTools(baseDir string) map[string]harness.Tool {
	return map[string]harness.Tool{
		"current_time":      tools.CurrentTimeTool{},
		"read_file":         tools.ReadFileTool{BaseDir: baseDir},
		"list_files":        tools.ListFilesTool{BaseDir: baseDir},
		"read_file_partial": tools.ReadPartialFileTool{BaseDir: baseDir},
		"get_file_size":     tools.GetFileSizeTool{BaseDir: baseDir},
		"edit_file":         tools.EditFileTool{BaseDir: baseDir},
		"write_file":        tools.WriteFileTool{BaseDir: baseDir},
		// Web tools (roadmap 2.3); AllowLocal stays false so workflows
		// can't reach internal addresses by default.
		"web_fetch":  tools.WebFetchTool{},
		"web_search": tools.WebSearchTool{},
	}
}

func (r *Runner) loadTools() error {
	all := builtinTools(r.wf.WorkDir)
	// MCP servers from config.yaml (roadmap 2.2) join the builtin pool
	// BEFORE name selection, so workflow.yaml `tools: builtin:` lists
	// and "all" can name discovered MCP tools.
	r.loadMCPTools(all)
	requested := r.wf.Tools.Builtin
	wantAll := false
	for _, n := range requested {
		if strings.EqualFold(n, "all") {
			wantAll = true
		}
	}
	if wantAll {
		for name, t := range all {
			r.tools[name] = t
		}
	} else {
		for _, n := range requested {
			if strings.EqualFold(n, "none") {
				continue
			}
			t, ok := all[n]
			if !ok {
				return fmt.Errorf("unknown builtin tool %q (available: %s)",
					n, strings.Join(sortedKeys(all), ", "))
			}
			r.tools[n] = t
		}
	}

	// Lua tools: explicit lua_dir, or auto-detect "tools/" in the workflow dir.
	luaDir := r.wf.Tools.LuaDir
	if luaDir == "" {
		if _, err := os.Stat(filepath.Join(r.wf.Dir, "tools")); err == nil {
			luaDir = "tools"
		}
	} else {
		resolved, err := r.wf.ResolvePath(luaDir)
		if err != nil {
			return fmt.Errorf("tools.lua_dir: %w", err)
		}
		luaDir = resolved
	}
	if luaDir == "" {
		return nil
	}
	if !filepath.IsAbs(luaDir) {
		luaDir = filepath.Join(r.wf.Dir, luaDir)
	}
	entries, err := os.ReadDir(luaDir)
	if err != nil {
		return fmt.Errorf("read lua tools dir %q: %w", luaDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".lua" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(luaDir, entry.Name()))
		if err != nil {
			return fmt.Errorf("read lua tool %s: %w", entry.Name(), err)
		}
		tool, err := luatool.NewLuaToolFromScriptWithConfig(string(data), luatool.FilesystemConfig{BaseDir: r.wf.WorkDir})
		if err != nil {
			return fmt.Errorf("lua tool %s: %w", entry.Name(), err)
		}
		r.luaTools = append(r.luaTools, tool)
		r.tools[tool.Name()] = tool
	}
	return nil
}

// loadMCPTools spawns every MCP server declared under config.yaml's
// mcp-servers key (roadmap 2.2) and merges each server's discovered
// tools into the all map, keyed by tool name. A name that collides with
// an already-loaded tool registers as "<server>_<name>" (same rule as
// mcp.LoadTools in the REPL). A failing server is a warning on r.out,
// never a workflow-load failure - the run proceeds without it. Clients
// are tracked on the Runner so Close terminates the server processes.
func (r *Runner) loadMCPTools(all map[string]harness.Tool) {
	conf := config.GetConfig()
	if conf == nil || len(conf.MCPServers) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, srv := range conf.MCPServers {
		client, _, err := mcp.StartServer(srv.Command, srv.Args, nil)
		if err != nil {
			fmt.Fprintf(r.out, "Warning: failed to start MCP server %q (%s): %v\n", srv.Name, srv.Command, err)
			continue
		}
		r.mcpClients = append(r.mcpClients, client)
		// Discover into a throwaway registry (so mcp.LoadTools'
		// collision logic runs per-server), then merge into all.
		tmp := harness.NewRegistry()
		n, err := mcp.LoadTools(ctx, client, tmp, srv.Name)
		if err != nil {
			fmt.Fprintf(r.out, "Warning: failed to load tools from MCP server %q: %v\n", srv.Name, err)
			continue
		}
		for _, tp := range tmp.Policies() {
			t, ok := tmp.Get(tp.Name)
			if !ok {
				continue
			}
			name := tp.Name
			if _, exists := all[name]; exists {
				prefixed := srv.Name + "_" + name
				fmt.Fprintf(r.out, "mcp: tool %q from server %q collides with an existing tool, registering as %q\n", name, srv.Name, prefixed)
				name = prefixed
			}
			all[name] = t
		}
		fmt.Fprintf(r.out, "Loaded %d MCP tools from server %q\n", n, srv.Name)
	}
}

// registryFor builds the per-step tool registry from a ToolSelector and
// stamps it with the config.yaml tool-permissions policies (roadmap
// 1.2). ApplyPolicies replaces the whole mapping, and ApplyPolicies
// with empty lists is a no-op keep of the default-allow behavior.
func (r *Runner) registryFor(sel ToolSelector) *harness.Registry {
	reg := harness.NewRegistry()
	reg.ApplyPolicies(r.permAllow, r.permAsk, r.permDeny)
	switch sel.Mode {
	case "none":
		return reg
	case "list":
		for _, n := range sel.Names {
			if t, ok := r.tools[n]; ok {
				reg.Register(t)
			}
			// Unknown names are skipped here; the agent loop reports a
			// "no such tool" error to the model if it calls one anyway.
		}
		return reg
	default: // "" or "all"
		for _, t := range r.tools {
			reg.Register(t)
		}
		return reg
	}
}

// -----------------------------------------------------------------------------
// Execution
// -----------------------------------------------------------------------------

// Run executes the workflow. If main.lua exists it takes over completely and
// drives everything through the Lua harness module; otherwise the steps loop
// runs. args become harness.args for main.lua. Returns the first fatal error.
func (r *Runner) Run(ctx context.Context, args []string) error {
	r.sess.args = args

	if r.wf.HasLuaMain() {
		fmt.Fprintf(r.out, "workflow %q: running %s\n", r.wf.Name, LuaMainName)
		return r.runLuaMain(ctx)
	}

	fmt.Fprintf(r.out, "workflow %q: %d step(s), model %q at %s\n",
		r.wf.Name, len(r.wf.Steps), r.cfg.Model, r.cfg.BaseURL)

	for i := range r.wf.Steps {
		st := &r.wf.Steps[i]
		fmt.Fprintf(r.out, "\n--- step %d/%d: %s ---\n", i+1, len(r.wf.Steps), st.Name)
		result, err := r.runStep(ctx, st)
		if err != nil {
			if st.OnError == "skip" {
				fmt.Fprintf(r.out, "step %q failed, skipping: %v\n", st.Name, err)
				continue
			}
			return fmt.Errorf("step %q: %w", st.Name, err)
		}
		_ = result // recorded in stepOutputs by runStep
	}
	fmt.Fprintf(r.out, "\nworkflow %q: done\n", r.wf.Name)
	return nil
}

// runStep executes one Step (shared by the steps loop and harness.run_step).
func (r *Runner) runStep(ctx context.Context, st *Step) (string, error) {
	// 1. Resolve the prompt text.
	prompt, err := r.stepPrompt(st)
	if err != nil {
		return "", err
	}

	// 2. Resolve media paths, validating they exist and are images the
	// providers can actually ingest (utils.LoadImage enforces the rest).
	var media []string
	for _, m := range st.Media {
		full, err := r.wf.ResolvePath(m)
		if err != nil {
			return "", fmt.Errorf("media: %w", err)
		}
		if _, err := os.Stat(full); err != nil {
			return "", fmt.Errorf("media file: %w", err)
		}
		if !utils.IsSupportedImage(full) {
			return "", fmt.Errorf("media file %q is not a supported image (png/jpg/jpeg/gif/webp)", m)
		}
		media = append(media, full)
	}

	// 3. Pre-script: may rewrite step.prompt / step.media.
	if st.PreScript != "" {
		var err error
		prompt, media, _, err = r.runStepScript(ctx, st.PreScript, st, prompt, media, nil)
		if err != nil {
			return "", fmt.Errorf("pre_script: %w", err)
		}
	}

	// 4. Run the agent turn against the active Harness.
	h, err := r.harnessForStep(st)
	if err != nil {
		return "", err
	}

	// Per-step model/temperature overrides apply only to this step's
	// requests; restore the base values afterwards.
	oldModel, oldTemp := h.Config.Model, h.Config.Temperature
	if st.Model != "" {
		h.Config.Model = st.Model
	}
	if st.Temperature != nil {
		h.Config.Temperature = *st.Temperature
	}

	// A step with a schema gets the JSON-Schema instructions appended to
	// its (already templated) prompt; the reply is parsed and validated
	// with one automatic retry (see askStructured).
	if st.Schema != nil {
		prompt += schemaInstructions(st.Schema)
	}

	result, decoded, err := r.askStructured(ctx, h, prompt, media, st.Schema)
	h.Config.Model, h.Config.Temperature = oldModel, oldTemp
	// Close out any unterminated thinking line before the result print so
	// the two never interleave on the output.
	if r.thinking != nil {
		r.thinking.flush()
	}
	if err != nil {
		return "", err
	}

	// 5. Post-script: may rewrite the result via the `result` global.
	if st.PostScript != "" {
		var err error
		_, _, newResult, err := r.runStepScript(ctx, st.PostScript, st, prompt, media, &result)
		if err != nil {
			return "", fmt.Errorf("post_script: %w", err)
		}
		result = newResult
	}

	// 6. Record and report. The raw reply text is recorded exactly as
	// before (so {{steps.name}} stays prose-compatible); the decoded
	// value additionally feeds {{steps.name.field}} and step_json().
	r.stepOutputs[st.Name] = result
	if st.SaveAs != "" {
		r.stepOutputs[st.SaveAs] = result
	}
	if st.Schema != nil {
		r.stepJSON[st.Name] = decoded
		if st.SaveAs != "" {
			r.stepJSON[st.SaveAs] = decoded
		}
	}
	fmt.Fprintln(r.out, result)
	reportUsage(r.out, h.LastUsage, h.Config.MaxContextTokens)
	return result, nil
}

// askStructured runs one step's model turn. Without a schema it is the
// plain ask (decoded is then nil). With one, the reply is parsed and
// validated against the schema; on parse OR validation failure the model
// is re-asked exactly once with the problems appended to the prompt. The
// retry reuses the same Harness, so the step's fresh/reused conversation
// semantics are unchanged. A second failure is an error whose message
// carries the validation problems; runStep's caller turns that into the
// step error, where on_error: stop|skip applies as usual.
func (r *Runner) askStructured(ctx context.Context, h *harness.Harness, prompt string, media []string, schema map[string]any) (string, any, error) {
	ask := func(p string) (string, error) {
		if len(media) > 0 {
			return h.AskWithImages(ctx, p, media)
		}
		return h.Ask(ctx, p)
	}
	reply, err := ask(prompt)
	if err != nil {
		return "", nil, err
	}
	if schema == nil {
		return reply, nil, nil
	}
	decoded, problems := checkStructuredReply(reply, schema)
	if len(problems) == 0 {
		return reply, decoded, nil
	}
	joined := strings.Join(problems, "; ")
	fmt.Fprintf(r.out, "step reply invalid, retrying once: %s\n", joined)
	retryPrompt := prompt + fmt.Sprintf(
		"\n\nYour previous reply was not valid. Problems: %s\nReply again with ONLY the JSON value.", joined)
	reply, err = ask(retryPrompt)
	if err != nil {
		return "", nil, err
	}
	decoded, problems = checkStructuredReply(reply, schema)
	if len(problems) > 0 {
		return "", nil, fmt.Errorf("reply failed schema validation after one retry: %s", strings.Join(problems, "; "))
	}
	return reply, decoded, nil
}

func (r *Runner) stepPrompt(st *Step) (string, error) {
	if st.Prompt != "" {
		return r.render(st.Prompt), nil
	}
	full, err := r.wf.ResolvePath(st.PromptFile)
	if err != nil {
		return "", fmt.Errorf("prompt_file: %w", err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("prompt_file: %w", err)
	}
	return r.render(string(data)), nil
}

// harnessForStep returns the Harness a step should use: a fresh one when the
// step declares its own system_prompt (fresh conversation = fresh persona),
// otherwise the shared active one, created lazily with the base config's
// system prompt. The step's tool selector is applied by swapping the
// registry on the shared harness.
func (r *Runner) harnessForStep(st *Step) (*harness.Harness, error) {
	reg := r.registryFor(st.Tools)
	if st.SystemPrompt != "" {
		cfg := r.cfg
		cfg.SystemPrompt = st.SystemPrompt
		r.sess.h = r.newHarness(cfg, reg)
		return r.sess.h, nil
	}
	if r.sess.h == nil {
		r.sess.h = r.newHarness(r.cfg, reg)
	} else {
		r.sess.h.Registry = reg
	}
	return r.sess.h, nil
}

// -----------------------------------------------------------------------------
// Templating
// -----------------------------------------------------------------------------

var tmplRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.-]+)\s*\}\}`)

// render expands {{var}} from workflow vars and {{steps.name}} from prior
// step outputs. When a schema step decoded a JSON value, dotted paths
// {{steps.name.field}} (and bare {{name.field}}) resolve inside it.
// Unknown placeholders are left untouched so prompts can contain literal
// braces without breaking.
func (r *Runner) render(s string) string {
	return tmplRe.ReplaceAllStringFunc(s, func(m string) string {
		key := tmplRe.FindStringSubmatch(m)[1]
		// {{steps.name}} (and bare {{name}}) resolve against prior step
		// outputs; {{vars.name}} (and bare {{name}}) against workflow vars.
		if out, ok := r.stepOutputs[key]; ok {
			return out
		}
		if rest := strings.TrimPrefix(key, "steps."); rest != key {
			if out, ok := r.stepOutputs[rest]; ok {
				return out
			}
			// {{steps.name.field}}: dotted path into the decoded JSON
			// of a schema step (roadmap 2.5), tried before the literal
			// fall-through so existing placeholders keep their byte-for-
			// byte behavior.
			if v, ok := lookupStepJSONField(r.stepJSON, rest); ok {
				return renderJSONValue(v)
			}
			return m
		}
		if v, ok := r.wf.Vars[key]; ok {
			return v
		}
		if rest := strings.TrimPrefix(key, "vars."); rest != key {
			if v, ok := r.wf.Vars[rest]; ok {
				return v
			}
			return m
		}
		// Bare {{name.field}} shorthand, same rule as {{steps.name.field}}.
		if v, ok := lookupStepJSONField(r.stepJSON, key); ok {
			return renderJSONValue(v)
		}
		return m
	})
}

// lookupStepJSONField resolves "name.field.path" against the decoded
// JSON values of schema steps: the first dotted segment is the step name
// (step names never contain dots), the rest walks map[string]any levels
// inside the decoded value.
func lookupStepJSONField(stepJSON map[string]any, key string) (any, bool) {
	i := strings.Index(key, ".")
	if i <= 0 {
		return nil, false
	}
	decoded, ok := stepJSON[key[:i]]
	if !ok {
		return nil, false
	}
	return lookupJSONField(decoded, key[i+1:])
}

// lookupJSONField walks a dotted field path through a decoded JSON value
// (descending through map[string]any levels). found=false on a missing
// key or a non-object segment.
func lookupJSONField(root any, path string) (any, bool) {
	cur := root
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[seg]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// renderJSONValue turns a decoded-JSON value into prompt text: strings
// verbatim, numbers without scientific notation (42, not 4.2e+01), and
// arrays/objects as compact JSON.
func renderJSONValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return formatFloatJSON(t)
	case map[string]any, []any, nil:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// formatFloatJSON renders integral float64s in plain decimal notation at
// any magnitude (JSON numbers decode to float64, so fmt.Sprint would
// print 42 as "42" but 4.2e20 as "4.2e+20"); non-integral values use the
// shortest round-trip form.
func formatFloatJSON(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	if f == math.Trunc(f) {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// -----------------------------------------------------------------------------
// Step pre/post scripts
// -----------------------------------------------------------------------------

// runStepScript executes a pre or post Lua script against a fresh, small VM.
// The script sees: step (table: name/prompt/media/vars/steps), log(...),
// plus the sandboxed filesystem and http modules. For post scripts, result
// is also passed in and read back (the script can reassign `result`).
// Returns the possibly rewritten prompt/media (and result for post).
func (r *Runner) runStepScript(ctx context.Context, script string, st *Step, prompt string, media []string, result *string) (string, []string, string, error) {
	_ = ctx
	full, err := r.wf.ResolvePath(script)
	if err != nil {
		return "", nil, "", err
	}

	L := lua.NewState()
	defer L.Close()
	luatool.RegisterHTTPModule(L, luatool.DefaultHTTPConfig)
	luatool.RegisterFilesystemModule(L, luatool.FilesystemConfig{BaseDir: r.wf.WorkDir})

	mediaLua := make([]any, len(media))
	for i, m := range media {
		mediaLua[i] = m
	}
	stepTbl := map[string]any{
		"name":   st.Name,
		"prompt": prompt,
		"media":  mediaLua,
		"vars":   mapAny(r.wf.Vars),
		"steps":  mapAny(r.stepOutputs),
	}
	L.SetGlobal("step", luatool.GoToLua(L, stepTbl))
	L.SetGlobal("log", L.NewFunction(func(L *lua.LState) int {
		fmt.Fprintln(r.out, strings.Join(luaArgsToStrings(L), " "))
		return 0
	}))
	if result != nil {
		L.SetGlobal("result", lua.LString(*result))
	}

	if err := L.DoFile(full); err != nil {
		return "", nil, "", err
	}

	// Read back mutated fields.
	outPrompt := prompt
	if v := L.GetGlobal("step"); v.Type() == lua.LTTable {
		tbl := v.(*lua.LTable)
		if p := tbl.RawGetString("prompt"); p.Type() == lua.LTString {
			outPrompt = p.String()
		}
		if m := tbl.RawGetString("media"); m.Type() == lua.LTTable {
			var rewritten []string
			if arr, ok := luatool.LuaToGo(m).([]any); ok {
				for _, item := range arr {
					rewritten = append(rewritten, fmt.Sprintf("%v", item))
				}
			}
			// Re-validate rewritten media paths.
			var resolved []string
			for _, m := range rewritten {
				full, err := r.wf.ResolvePath(m)
				if err != nil {
					return "", nil, "", fmt.Errorf("media: %w", err)
				}
				resolved = append(resolved, full)
			}
			media = resolved
		}
	}
	if result != nil {
		if v := L.GetGlobal("result"); v.Type() == lua.LTString {
			*result = v.String()
		}
		newResult := ""
		if result != nil {
			newResult = *result
		}
		return outPrompt, media, newResult, nil
	}
	return outPrompt, media, "", nil
}

// -----------------------------------------------------------------------------
// Shared helpers
// -----------------------------------------------------------------------------

func reportUsage(out io.Writer, u harness.Usage, maxCtx int) {
	switch {
	case u.TotalTokens == 0:
		fmt.Fprintln(out, "[context: server did not report token usage]")
	case maxCtx > 0:
		pct := 100 * u.PromptTokens / maxCtx
		fmt.Fprintf(out, "[context: %d / %d tokens (%d%%)]\n", u.PromptTokens, maxCtx, pct)
	default:
		fmt.Fprintf(out, "[context: %d prompt + %d completion = %d tokens]\n",
			u.PromptTokens, u.CompletionTokens, u.TotalTokens)
	}
}

func mapAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// simple insertion sort, the maps are tiny
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func luaArgsToStrings(L *lua.LState) []string {
	parts := make([]string, 0, L.GetTop())
	for i := 1; i <= L.GetTop(); i++ {
		parts = append(parts, L.Get(i).String())
	}
	return parts
}
