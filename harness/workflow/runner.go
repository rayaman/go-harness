package workflow

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"ai-harness/harness"
	luatool "ai-harness/harness/lua-tool"
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

	// tools holds every tool the workflow loaded (builtin + Lua), keyed
	// by name. Per-step `tools:` filters select a subset from this map.
	tools    map[string]harness.Tool
	luaTools []*luatool.LuaTool // kept so Close can release their VMs

	out io.Writer

	// stepOutputs maps step names (and save_as aliases) to their final
	// text output, for {{steps.name}} templating and step_results().
	stepOutputs map[string]string

	sess *session
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
	r := &Runner{
		wf:          wf,
		cfg:         wf.MergeConfig(base),
		out:         out,
		tools:       map[string]harness.Tool{},
		stepOutputs: map[string]string{},
	}
	r.client = harness.NewClient(r.cfg.BaseURL, r.cfg.APIKey)

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
	r.sess = &session{r: r}
	return r, nil
}

// Close releases any Lua tool VMs held by the runner.
func (r *Runner) Close() {
	for _, t := range r.luaTools {
		t.Close()
	}
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
	}
}

func (r *Runner) loadTools() error {
	all := builtinTools(r.wf.WorkDir)
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

// registryFor builds the per-step tool registry from a ToolSelector.
func (r *Runner) registryFor(sel ToolSelector) *harness.Registry {
	reg := harness.NewRegistry()
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

	var result string
	if len(media) > 0 {
		result, err = h.AskWithImages(ctx, prompt, media)
	} else {
		result, err = h.Ask(ctx, prompt)
	}
	h.Config.Model, h.Config.Temperature = oldModel, oldTemp
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

	// 6. Record and report.
	r.stepOutputs[st.Name] = result
	if st.SaveAs != "" {
		r.stepOutputs[st.SaveAs] = result
	}
	fmt.Fprintln(r.out, result)
	reportUsage(r.out, h.LastUsage, h.Config.MaxContextTokens)
	return result, nil
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
		r.sess.h = harness.New(cfg, r.client, reg)
		return r.sess.h, nil
	}
	if r.sess.h == nil {
		r.sess.h = harness.New(r.cfg, r.client, reg)
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
// step outputs. Unknown placeholders are left untouched so prompts can
// contain literal braces without breaking.
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
		return m
	})
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
