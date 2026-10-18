// Package workflow implements directory-based, declarative agent workflows
// that can be run instead of the interactive REPL:
//
//	go run main.go --workflow path/to/workflow
//
// A workflow directory looks like:
//
//	my_workflow/
//	├── workflow.yaml   # required: config overrides + vars + steps
//	├── main.lua        # optional: programmatic mode (overrides the steps loop)
//	├── prompts/        # optional: prompt_file targets
//	├── assets/         # optional: media attachments
//	├── scripts/        # optional: per-step pre/post Lua scripts
//	└── tools/          # optional: per-workflow Lua tools
//
// loader.go parses and validates workflow.yaml and merges its config
// overrides onto a base harness.Config. See runner.go for execution and
// lua_api.go for the Lua "harness" module.
package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"ai-harness/harness"
)

// ManifestName is the required YAML file inside a workflow directory.
const ManifestName = "workflow.yaml"

// LuaMainName is the optional entry point that switches a workflow from the
// declarative steps loop to fully programmatic Lua control.
const LuaMainName = "main.lua"

// Workflow is the parsed contents of workflow.yaml plus the resolved
// workflow directory on disk.
type Workflow struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	WorkDir     string            `yaml:"work_dir"` // BaseDir for builtin tools; default: workflow dir
	Vars        map[string]string `yaml:"vars"`
	Config      ConfigOverrides   `yaml:"config"`
	Tools       ToolsSpec         `yaml:"tools"`
	Steps       []Step            `yaml:"steps"`

	// Dir is the absolute path of the workflow directory. All relative
	// paths in the manifest (prompt_file, media, scripts, lua_dir) are
	// resolved against it. Set by Load, never read from YAML.
	Dir string `yaml:"-"`
}

// ConfigOverrides is a partial harness.Config. Pointer fields distinguish
// "not set" from "set to the zero value", so a workflow can, for example,
// set temperature to 0 without clobbering the base model.
type ConfigOverrides struct {
	Provider          *string  `yaml:"provider"`
	BaseURL           *string  `yaml:"base_url"`
	Model             *string  `yaml:"model"`
	APIKey            *string  `yaml:"api_key"`
	SystemPrompt      *string  `yaml:"system_prompt"`
	Temperature       *float64 `yaml:"temperature"`
	MaxToolIterations *int     `yaml:"max_tool_iterations"`
	MaxContextTokens  *int     `yaml:"max_context_tokens"`
}

// ToolsSpec selects the tools a workflow registers into its Registry.
type ToolsSpec struct {
	// Builtin selects Go tools by name. A single "all" enables every
	// builtin; "none" (or empty) enables none. Unknown names are a load
	// error so typos fail loudly instead of silently doing nothing.
	Builtin []string `yaml:"builtin"`
	// LuaDir is a directory of .lua tools (relative to the workflow dir),
	// loaded with the existing luatool machinery. Empty = "tools" if that
	// directory exists.
	LuaDir string `yaml:"lua_dir"`
}

// Step is one unit of work in the declarative steps loop.
type Step struct {
	Name string `yaml:"name"` // unique; result recorded as steps.<name>

	// Prompt is an inline prompt; PromptFile reads it from a file under
	// the workflow dir. Exactly one is required. Both go through
	// {{...}} templating (see RenderTemplate).
	Prompt     string `yaml:"prompt"`
	PromptFile string `yaml:"prompt_file"`

	SystemPrompt string `yaml:"system_prompt"` // starts a fresh conversation with this persona

	Model       string   `yaml:"model"`       // per-step model override
	Temperature *float64 `yaml:"temperature"` // per-step sampling override
	Media       []string `yaml:"media"`       // image paths relative to the workflow dir

	Tools ToolSelector `yaml:"tools"` // per-step tool filter (default: all loaded)

	PreScript  string `yaml:"pre_script"`  // Lua run before the model call (may rewrite step.prompt/media)
	PostScript string `yaml:"post_script"` // Lua run after (may rewrite the recorded result)

	SaveAs  string `yaml:"save_as"`  // alias for this step's output in steps.*
	OnError string `yaml:"on_error"` // "stop" (default) | "skip"
}

// ToolSelector accepts either a bare string ("all" / "none") or a list of
// tool names in YAML:
//
//	tools: none
//	tools: [read_file, current_time]
type ToolSelector struct {
	Mode  string   // "" (all) | "all" | "none" | "list"
	Names []string // populated when Mode == "list"
}

// UnmarshalYAML implements the string-or-list union above.
func (s *ToolSelector) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.SequenceNode {
		var names []string
		if err := node.Decode(&names); err != nil {
			return err
		}
		s.Mode = "list"
		s.Names = names
		return nil
	}
	var mode string
	if err := node.Decode(&mode); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "all":
		s.Mode = "all"
	case "none":
		s.Mode = "none"
	default:
		return fmt.Errorf("tools must be \"all\", \"none\", or a list of tool names (got %q)", mode)
	}
	return nil
}

var stepNameRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// Load reads and validates workflow.yaml from dir. It does not touch the
// model server or build any Harness - that's the Runner's job.
func Load(dir string) (*Workflow, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("workflow directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workflow path %q is not a directory", dir)
	}

	data, err := os.ReadFile(filepath.Join(abs, ManifestName))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ManifestName, err)
	}

	wf := &Workflow{}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // unknown keys = error, so typos fail loudly
	if err := dec.Decode(wf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ManifestName, err)
	}
	wf.Dir = abs

	if wf.Name == "" {
		wf.Name = filepath.Base(abs)
	}
	if wf.WorkDir == "" {
		wf.WorkDir = abs
	} else if !filepath.IsAbs(wf.WorkDir) {
		wf.WorkDir = filepath.Join(abs, wf.WorkDir)
	}

	wf.Vars = expandEnvMap(wf.Vars)
	wf.Config.expandEnv()

	if err := wf.validate(); err != nil {
		return nil, fmt.Errorf("invalid workflow %q: %w", wf.Name, err)
	}
	return wf, nil
}

func (wf *Workflow) validate() error {
	seen := map[string]bool{}
	for i := range wf.Steps {
		st := &wf.Steps[i]
		if st.Name == "" {
			return fmt.Errorf("step %d: name is required", i+1)
		}
		if !stepNameRe.MatchString(st.Name) {
			return fmt.Errorf("step %q: name must match %s", st.Name, stepNameRe.String())
		}
		if seen[st.Name] {
			return fmt.Errorf("step %q: duplicate step name", st.Name)
		}
		seen[st.Name] = true

		if (st.Prompt == "") == (st.PromptFile == "") {
			return fmt.Errorf("step %q: exactly one of prompt or prompt_file is required", st.Name)
		}
		switch st.OnError {
		case "", "stop", "skip":
		default:
			return fmt.Errorf("step %q: on_error must be \"stop\" or \"skip\" (got %q)", st.Name, st.OnError)
		}
	}

	if len(wf.Steps) == 0 && !wf.HasLuaMain() {
		return fmt.Errorf("workflow defines no steps and has no %s to run", LuaMainName)
	}
	return nil
}

// HasLuaMain reports whether the workflow ships a main.lua entry point.
func (wf *Workflow) HasLuaMain() bool {
	st, err := os.Stat(filepath.Join(wf.Dir, LuaMainName))
	return err == nil && !st.IsDir()
}

// LuaMainPath returns the absolute path to main.lua.
func (wf *Workflow) LuaMainPath() string {
	return filepath.Join(wf.Dir, LuaMainName)
}

// MergeConfig overlays the workflow's config block onto base and returns the
// resulting Config. base is typically harness.OllamaConfig() or
// harness.DefaultConfig().
func (wf *Workflow) MergeConfig(base harness.Config) harness.Config {
	out := base
	c := wf.Config
	if c.Provider != nil {
		out.Provider = *c.Provider
	}
	if c.BaseURL != nil {
		out.BaseURL = *c.BaseURL
	}
	if c.Model != nil {
		out.Model = *c.Model
	}
	if c.APIKey != nil {
		out.APIKey = *c.APIKey
	}
	if c.SystemPrompt != nil {
		out.SystemPrompt = *c.SystemPrompt
	}
	if c.Temperature != nil {
		out.Temperature = *c.Temperature
	}
	if c.MaxToolIterations != nil {
		out.MaxToolIterations = *c.MaxToolIterations
	}
	if c.MaxContextTokens != nil {
		out.MaxContextTokens = *c.MaxContextTokens
	}
	return out
}

// ResolvePath maps a path declared in workflow.yaml (prompt file, media
// file, script, ...) to an absolute path under the workflow directory.
// Absolute inputs pass through; relative inputs are joined onto wf.Dir and
// rejected if they escape it.
func (wf *Workflow) ResolvePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(p) {
		return p, nil
	}
	full := filepath.Join(wf.Dir, p)
	rel, err := filepath.Rel(wf.Dir, full)
	if err != nil {
		return "", fmt.Errorf("invalid path %q: %w", p, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workflow directory", p)
	}
	return full, nil
}

// -----------------------------------------------------------------------------
// ${ENV_VAR} expansion
// -----------------------------------------------------------------------------

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${NAME} occurrences with the value of the environment
// variable NAME. Use it for api_key/base_url so real secrets never live in
// YAML. Unset variables expand to "" (same as shell behavior for : "${X}").
func expandEnv(s string) string {
	return envRe.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(envRe.FindStringSubmatch(m)[1])
	})
}

func expandEnvMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = expandEnv(v)
	}
	return out
}

func (c *ConfigOverrides) expandEnv() {
	for _, p := range []*string{c.Provider, c.BaseURL, c.Model, c.APIKey, c.SystemPrompt} {
		if p != nil {
			*p = expandEnv(*p)
		}
	}
}
