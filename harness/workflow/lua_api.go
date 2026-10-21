package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"ai-harness/harness"
	luatool "ai-harness/harness/lua-tool"
	"ai-harness/harness/utils"
)

// lua_api.go exposes the "harness" Lua module consumed by a workflow's
// main.lua (and require-able from step scripts). It turns the Runner's
// agent loop, model-server introspection, and step results into plain Lua
// functions.
//
// Available in main.lua:
//
//	harness.ask(prompt)                        -> text, err
//	harness.ask(prompt, {model=, temperature=, media={paths}, system=, reset=, tools=})
//	harness.ask_media(prompt, {"a.png"})       -> text, err
//	harness.run_step("name")                   -> text, err   (YAML-declared step)
//	harness.list_models()                      -> {{id=, modified_at=, size=, size_human=}, ...}, err
//	harness.context_length("model")            -> n, err
//	harness.set_system_prompt("persona")       // resets the shared conversation
//	harness.set_model("model")                 // changes the default model
//	harness.step_results()                     -> {step_name = output, ...}
//	harness.step_json(name)                    -> decoded JSON table for a
//	                                             schema step, nil if none
//	                                             (step_results() keeps
//	                                             returning raw text only,
//	                                             for backward compat)
//	harness.vars                               -> workflow vars table
//	harness.args                               -> extra CLI args table
//	harness.log(...)                           -> stdout logging
//	harness.external.abspath(p)                -> absolute path, err
//	harness.external.exists(p)                 -> bool, err
//	harness.external.is_dir(p)                 -> bool, err
//	harness.external.mkdir(p)                  -> created dir, err (MkdirAll)
//	harness.external.list_images(dir)          -> sorted absolute image paths, err
//	harness.external.read_file(p)              -> text, err
//	harness.external.write_file(p, content)    -> message, err (parents auto-created)
//	harness.external.sleep(seconds)            -> nil, err (backoff helper for retry loops)
//	json.encode(t) / json.decode(s)            -> table <-> JSON string
//
// Plus the sandboxed require("http") and require("filesystem") modules from
// the existing luatool package. harness.external is the deliberate
// sandbox-bypass sibling of "filesystem" - see newExternalTable.

func (r *Runner) runLuaMain(ctx context.Context) error {
	L := lua.NewState()
	defer L.Close()

	luatool.RegisterHTTPModule(L, luatool.DefaultHTTPConfig)
	luatool.RegisterFilesystemModule(L, luatool.FilesystemConfig{BaseDir: r.wf.WorkDir})
	registerJSONModule(L)
	r.registerHarnessModule(L, ctx)

	if err := L.DoFile(r.wf.LuaMainPath()); err != nil {
		return fmt.Errorf("%s: %w", LuaMainName, err)
	}
	return nil
}

// registerHarnessModule builds the harness table and attaches it both as a
// preloadable module (require("harness")) and as a global, since main.lua
// scripts read better without ceremony.
func (r *Runner) registerHarnessModule(L *lua.LState, ctx context.Context) {
	mk := func(L *lua.LState) int {
		tbl := L.NewTable()
		L.SetFuncs(tbl, map[string]lua.LGFunction{
			"ask":               r.luaAsk(ctx),
			"ask_media":         r.luaAskMedia(ctx),
			"run_step":          r.luaRunStep(ctx),
			"list_models":       r.luaListModels(ctx),
			"context_length":    r.luaContextLength(ctx),
			"set_system_prompt": r.luaSetSystemPrompt,
			"set_model":         r.luaSetModel,
			"step_results":      r.luaStepResults,
			"step_json":         r.luaStepJSON,
			"log":               r.luaLog,
		})
		tbl.RawSetString("external", newExternalTable(L))
		tbl.RawSetString("vars", luatool.GoToLua(L, mapAny(r.wf.Vars)))
		argsLua := make([]any, len(r.sess.args))
		for i, a := range r.sess.args {
			argsLua[i] = a
		}
		tbl.RawSetString("args", luatool.GoToLua(L, argsLua))
		L.Push(tbl)
		return 1
	}

	L.PreloadModule("harness", mk)
	mk(L)
	L.SetGlobal("harness", L.Get(-1))
	L.Pop(1)
}

// -----------------------------------------------------------------------------
// ask / ask_media
// -----------------------------------------------------------------------------

// askOpts mirrors the per-call options accepted by harness.ask's second
// argument.
type askOpts struct {
	Model       string
	Temperature *float64
	Media       []string
	System      string
	Reset       bool
	Tools       ToolSelector
}

// resolveMedia converts a Lua array of path strings into validated absolute
// paths: each must stay inside the workflow dir, exist, and be a supported
// image (the same rules the declarative media: field enforces).
func (r *Runner) resolveMedia(val lua.LValue) ([]string, error) {
	arr, ok := luatool.LuaToGo(val).([]any)
	if !ok {
		return nil, fmt.Errorf("media must be an array of file paths")
	}
	var out []string
	for _, item := range arr {
		full, err := r.wf.ResolvePath(fmt.Sprintf("%v", item))
		if err != nil {
			return nil, fmt.Errorf("media: %w", err)
		}
		if _, err := os.Stat(full); err != nil {
			return nil, fmt.Errorf("media file: %w", err)
		}
		if !utils.IsSupportedImage(full) {
			return nil, fmt.Errorf("media file %q is not a supported image (png/jpg/jpeg/gif/webp)", full)
		}
		out = append(out, full)
	}
	return out, nil
}

func (r *Runner) parseAskOpts(L *lua.LState, idx int) (askOpts, error) {
	var o askOpts
	tbl, ok := L.Get(idx).(*lua.LTable)
	if !ok {
		return o, fmt.Errorf("options must be a table")
	}
	if v := tbl.RawGetString("model"); v.Type() == lua.LTString {
		o.Model = v.String()
	}
	if v := tbl.RawGetString("temperature"); v.Type() == lua.LTNumber {
		t := float64(v.(lua.LNumber))
		o.Temperature = &t
	}
	if v := tbl.RawGetString("system"); v.Type() == lua.LTString {
		o.System = v.String()
	}
	if v := tbl.RawGetString("reset"); v.Type() == lua.LTBool {
		o.Reset = bool(v.(lua.LBool))
	}
	if v := tbl.RawGetString("media"); v.Type() == lua.LTTable {
		media, err := r.resolveMedia(v)
		if err != nil {
			return o, err
		}
		o.Media = media
	}
	if v := tbl.RawGetString("tools"); v.Type() == lua.LTString {
		switch v.String() {
		case "none", "all":
			o.Tools.Mode = v.String()
		default:
			return o, fmt.Errorf(`tools option must be "none", "all", or a list of names`)
		}
	} else if v.Type() == lua.LTTable {
		if arr, ok := luatool.LuaToGo(v).([]any); ok {
			o.Tools.Mode = "list"
			for _, item := range arr {
				o.Tools.Names = append(o.Tools.Names, fmt.Sprintf("%v", item))
			}
		}
	}
	return o, nil
}

// doAsk is the shared core behind ask/ask_media: template the prompt, pick
// (or reset) the active harness, apply per-call overrides, and run the
// agent loop.
func (r *Runner) doAsk(ctx context.Context, prompt string, opts askOpts) (string, error) {
	h := r.harnessForOpts(opts.System, opts.Reset, opts.Tools)

	oldModel, oldTemp := h.Config.Model, h.Config.Temperature
	if opts.Model != "" {
		h.Config.Model = opts.Model
	}
	if opts.Temperature != nil {
		h.Config.Temperature = *opts.Temperature
	}

	var result string
	var err error
	if len(opts.Media) > 0 {
		result, err = h.AskWithImages(ctx, r.render(prompt), opts.Media)
	} else {
		result, err = h.Ask(ctx, r.render(prompt))
	}
	h.Config.Model, h.Config.Temperature = oldModel, oldTemp
	return result, err
}

func (r *Runner) luaAsk(ctx context.Context) lua.LGFunction {
	return func(L *lua.LState) int {
		prompt := L.CheckString(1)
		var opts askOpts
		if L.GetTop() >= 2 && L.Get(2).Type() != lua.LTNil {
			var err error
			opts, err = r.parseAskOpts(L, 2)
			if err != nil {
				return pushLuaError(L, err)
			}
		}
		result, err := r.doAsk(ctx, prompt, opts)
		if err != nil {
			return pushLuaError(L, err)
		}
		L.Push(lua.LString(result))
		L.Push(lua.LNil)
		return 2
	}
}

func (r *Runner) luaAskMedia(ctx context.Context) lua.LGFunction {
	return func(L *lua.LState) int {
		prompt := L.CheckString(1)
		media, err := r.resolveMedia(L.CheckAny(2))
		if err != nil {
			return pushLuaError(L, err)
		}
		opts := askOpts{Media: media}
		// Optional third table carries the same model/temperature/system
		// options as harness.ask's opts table.
		if L.GetTop() >= 3 && L.Get(3).Type() == lua.LTTable {
			extra, err := r.parseAskOpts(L, 3)
			if err != nil {
				return pushLuaError(L, err)
			}
			extra.Media = media
			opts = extra
		}
		result, err := r.doAsk(ctx, prompt, opts)
		if err != nil {
			return pushLuaError(L, err)
		}
		L.Push(lua.LString(result))
		L.Push(lua.LNil)
		return 2
	}
}

// -----------------------------------------------------------------------------
// run_step / models / state
// -----------------------------------------------------------------------------

func (r *Runner) luaRunStep(ctx context.Context) lua.LGFunction {
	return func(L *lua.LState) int {
		name := L.CheckString(1)
		for i := range r.wf.Steps {
			if r.wf.Steps[i].Name == name {
				result, err := r.runStep(ctx, &r.wf.Steps[i])
				if err != nil {
					return pushLuaError(L, err)
				}
				L.Push(lua.LString(result))
				L.Push(lua.LNil)
				return 2
			}
		}
		return pushLuaError(L, fmt.Errorf("no such step %q (declared: %s)",
			name, strings.Join(stepNames(r.wf.Steps), ", ")))
	}
}

func (r *Runner) luaListModels(ctx context.Context) lua.LGFunction {
	return func(L *lua.LState) int {
		models, err := r.client.ListModels(ctx)
		if err != nil {
			return pushLuaError(L, err)
		}
		out := make([]any, len(models))
		for i, m := range models {
			out[i] = map[string]any{
				"id":          m.ID,
				"modified_at": m.ModifiedAt,
				"size":        float64(m.SizeBytes),
				"size_human":  humanBytes(m.SizeBytes),
			}
		}
		L.Push(luatool.GoToLua(L, out))
		L.Push(lua.LNil)
		return 2
	}
}

func (r *Runner) luaContextLength(ctx context.Context) lua.LGFunction {
	return func(L *lua.LState) int {
		model := L.OptString(1, r.cfg.Model)
		n, err := r.client.ContextLength(ctx, model)
		if err != nil {
			return pushLuaError(L, err)
		}
		L.Push(lua.LNumber(n))
		L.Push(lua.LNil)
		return 2
	}
}

func (r *Runner) luaSetSystemPrompt(L *lua.LState) int {
	r.cfg.SystemPrompt = L.CheckString(1)
	r.sess.h = nil // force the next ask/step to build a fresh harness
	L.Push(lua.LString("system prompt set; conversation history reset"))
	return 1
}

func (r *Runner) luaSetModel(L *lua.LState) int {
	r.cfg.Model = L.CheckString(1)
	L.Push(lua.LString("default model set to " + r.cfg.Model))
	return 1
}

func (r *Runner) luaStepResults(L *lua.LState) int {
	L.Push(luatool.GoToLua(L, mapAny(r.stepOutputs)))
	return 1
}

// luaStepJSON returns the decoded JSON value of a schema-validated step
// (roadmap 2.5) as a Lua table - numbers -> number, strings -> string,
// bools -> bool, arrays -> 1-based tables, objects -> keyed tables - or
// nil when the step had no schema / no decoded value. step_results()
// deliberately keeps returning name -> raw text exactly as before:
// existing workflows read outputs as strings, so nesting a .json table
// under it (the roadmap's original wording) would break them.
func (r *Runner) luaStepJSON(L *lua.LState) int {
	name := L.CheckString(1)
	decoded, ok := r.stepJSON[name]
	if !ok {
		L.Push(lua.LNil)
		return 1
	}
	L.Push(luatool.GoToLua(L, decoded))
	return 1
}

func (r *Runner) luaLog(L *lua.LState) int {
	fmt.Fprintln(r.out, strings.Join(luaArgsToStrings(L), " "))
	return 0
}

// -----------------------------------------------------------------------------
// harness.external - sandbox bypass for human-supplied paths
// -----------------------------------------------------------------------------

// newExternalTable builds harness.external: path/file helpers that
// deliberately skip the workflow-dir sandbox. They exist so a main.lua can
// work with directories the *human* chose (an input image folder, an
// output folder) instead of paths the model picked. The safety model is
// provenance, not path validation: this table is attached only inside the
// main.lua VM (runLuaMain), never in the model's tool registry, so
// model-chosen tool arguments can never reach these functions. main.lua
// scripts are trusted code run by the user; they must treat the paths the
// user typed as legitimate and keep model-derived text out of them.
func newExternalTable(L *lua.LState) *lua.LTable {
	tbl := L.NewTable()
	L.SetFuncs(tbl, map[string]lua.LGFunction{
		"abspath":     luaExtAbsPath,
		"exists":      luaExtExists,
		"is_dir":      luaExtIsDir,
		"mkdir":       luaExtMkdir,
		"list_images": luaExtListImages,
		"read_file":   luaExtReadFile,
		"write_file":  luaExtWriteFile,
		"sleep":       luaExtSleep,
	})
	return tbl
}

// extAbs resolves a user-supplied path to an absolute one, relative to the
// process working directory. filepath.Abs already cleans the result.
func extAbs(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	return filepath.Abs(p)
}

func luaExtAbsPath(L *lua.LState) int {
	full, err := extAbs(L.CheckString(1))
	if err != nil {
		return pushLuaError(L, err)
	}
	L.Push(lua.LString(full))
	L.Push(lua.LNil)
	return 2
}

// luaExtExists reports false (with a nil error) for missing paths; only
// stat errors other than "not found" become errors.
func luaExtExists(L *lua.LState) int {
	full, err := extAbs(L.CheckString(1))
	if err != nil {
		return pushLuaError(L, err)
	}
	if _, err := os.Stat(full); err != nil {
		if os.IsNotExist(err) {
			L.Push(lua.LFalse)
			L.Push(lua.LNil)
			return 2
		}
		return pushLuaError(L, err)
	}
	L.Push(lua.LTrue)
	L.Push(lua.LNil)
	return 2
}

func luaExtIsDir(L *lua.LState) int {
	full, err := extAbs(L.CheckString(1))
	if err != nil {
		return pushLuaError(L, err)
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			L.Push(lua.LFalse)
			L.Push(lua.LNil)
			return 2
		}
		return pushLuaError(L, err)
	}
	L.Push(lua.LBool(info.IsDir()))
	L.Push(lua.LNil)
	return 2
}

func luaExtMkdir(L *lua.LState) int {
	full, err := extAbs(L.CheckString(1))
	if err != nil {
		return pushLuaError(L, err)
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		return pushLuaError(L, err)
	}
	L.Push(lua.LString(full))
	L.Push(lua.LNil)
	return 2
}

// luaExtListImages walks a directory recursively and returns every file
// with a supported image extension (utils.IsSupportedImage), sorted by
// absolute path so page order follows filename order.
func luaExtListImages(L *lua.LState) int {
	full, err := extAbs(L.CheckString(1))
	if err != nil {
		return pushLuaError(L, err)
	}
	info, err := os.Stat(full)
	if err != nil {
		return pushLuaError(L, err)
	}
	if !info.IsDir() {
		return pushLuaError(L, fmt.Errorf("%s is not a directory", full))
	}
	var found []string
	walkErr := filepath.Walk(full, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() && utils.IsSupportedImage(path) {
			found = append(found, path)
		}
		return nil
	})
	if walkErr != nil {
		return pushLuaError(L, walkErr)
	}
	sort.Strings(found)
	items := make([]any, len(found))
	for i, p := range found {
		items[i] = p
	}
	L.Push(luatool.GoToLua(L, items))
	L.Push(lua.LNil)
	return 2
}

func luaExtReadFile(L *lua.LState) int {
	full, err := extAbs(L.CheckString(1))
	if err != nil {
		return pushLuaError(L, err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return pushLuaError(L, err)
	}
	L.Push(lua.LString(data))
	L.Push(lua.LNil)
	return 2
}

// luaExtWriteFile writes content to any path outside the sandbox, creating
// parent directories. This intentionally bypasses the workflow-dir
// sandbox; see newExternalTable for why that is considered safe.
func luaExtWriteFile(L *lua.LState) int {
	full, err := extAbs(L.CheckString(1))
	if err != nil {
		return pushLuaError(L, err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return pushLuaError(L, err)
	}
	content := L.CheckString(2)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return pushLuaError(L, err)
	}
	L.Push(lua.LString(fmt.Sprintf("wrote %d bytes to %s", len(content), full)))
	L.Push(lua.LNil)
	return 2
}

// luaExtSleep blocks the main.lua thread for the given number of seconds.
// It exists because gopher-lua's oslib has no sleep; main.lua retry loops
// use it for backoff between model calls. Negative seconds are rejected.
func luaExtSleep(L *lua.LState) int {
	secs := L.CheckNumber(1)
	if secs < 0 {
		return pushLuaError(L, fmt.Errorf("sleep: seconds must be >= 0 (got %v)", float64(secs)))
	}
	time.Sleep(time.Duration(float64(secs) * float64(time.Second)))
	L.Push(lua.LNil)
	L.Push(lua.LNil)
	return 2
}

// -----------------------------------------------------------------------------
// json module
// -----------------------------------------------------------------------------

func registerJSONModule(L *lua.LState) {
	tbl := L.NewTable()
	L.SetFuncs(tbl, map[string]lua.LGFunction{
		"encode": func(L *lua.LState) int {
			goVal := luatool.LuaToGo(L.CheckAny(1))
			b, err := json.Marshal(goVal)
			if err != nil {
				return pushLuaError(L, err)
			}
			L.Push(lua.LString(string(b)))
			L.Push(lua.LNil)
			return 2
		},
		"decode": func(L *lua.LState) int {
			var goVal any
			if err := json.Unmarshal([]byte(L.CheckString(1)), &goVal); err != nil {
				return pushLuaError(L, err)
			}
			L.Push(luatool.GoToLua(L, goVal))
			L.Push(lua.LNil)
			return 2
		},
	})
	L.SetGlobal("json", tbl)
	L.PreloadModule("json", func(L *lua.LState) int {
		L.Push(tbl)
		return 1
	})
}

// -----------------------------------------------------------------------------
// harness selection + misc helpers
// -----------------------------------------------------------------------------

// harnessForOpts returns the Harness a Lua call should use: a fresh one
// when system is set or reset requested (fresh conversation = fresh
// persona), otherwise the shared active one so conversation history
// carries across asks and steps.
func (r *Runner) harnessForOpts(system string, reset bool, sel ToolSelector) *harness.Harness {
	reg := r.registryFor(sel)
	if system != "" || reset {
		cfg := r.cfg
		if system != "" {
			cfg.SystemPrompt = system
		}
		r.sess.h = r.newHarness(cfg, reg)
		return r.sess.h
	}
	if r.sess.h == nil {
		r.sess.h = r.newHarness(r.cfg, reg)
	} else {
		r.sess.h.Registry = reg
	}
	return r.sess.h
}

// pushLuaError pushes the (nil, err) return convention used by every
// function in the harness/json modules and returns the result count.
func pushLuaError(L *lua.LState, err error) int {
	L.Push(lua.LNil)
	L.Push(lua.LString(err.Error()))
	return 2
}

func stepNames(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
