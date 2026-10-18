package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ai-harness/harness"
)

func writeWorkflow(t *testing.T, yamlBody string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

func f64(v float64) *float64 { return &v }
func str(v string) *string   { return &v }
func iptr(v int) *int        { return &v }

func TestLoadValid(t *testing.T) {
	dir := writeWorkflow(t, `
name: demo
description: a demo
vars:
  topic: hello
config:
  model: m2
  temperature: 0.1
  max_context_tokens: 4096
tools:
  builtin: [current_time]
  lua_dir: ""
steps:
  - name: one
    prompt: "say {{topic}}"
    media: [a.png]
    tools: none
  - name: two
    prompt_file: prompts/two.txt
    system_prompt: persona
    save_as: alias
    on_error: skip
    tools: [read_file, current_time]
`)
	wf, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wf.Name != "demo" || len(wf.Steps) != 2 {
		t.Fatalf("unexpected parse: %+v", wf)
	}
	if wf.Config.Model == nil || *wf.Config.Model != "m2" {
		t.Errorf("model override not parsed")
	}
	if wf.Config.Temperature == nil || *wf.Config.Temperature != 0.1 {
		t.Errorf("temperature override not parsed")
	}
	if wf.Steps[0].Tools.Mode != "none" {
		t.Errorf("step tools string form: got %q", wf.Steps[0].Tools.Mode)
	}
	if wf.Steps[1].Tools.Mode != "list" || len(wf.Steps[1].Tools.Names) != 2 {
		t.Errorf("step tools list form: %+v", wf.Steps[1].Tools)
	}
	if wf.WorkDir != dir {
		t.Errorf("WorkDir default: got %q want %q", wf.WorkDir, dir)
	}
}

func TestLoadMergeConfig(t *testing.T) {
	dir := writeWorkflow(t, "name: m\nconfig:\n  model: over\n  temperature: 0\nsteps:\n  - name: a\n    prompt: x\n")
	wf, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	base := harness.Config{Model: "base", Temperature: 0.9, Provider: "ollama", MaxToolIterations: 5}
	got := wf.MergeConfig(base)
	if got.Model != "over" {
		t.Errorf("model: got %q", got.Model)
	}
	if got.Temperature != 0 {
		t.Errorf("explicit zero temperature should override, got %v", got.Temperature)
	}
	if got.Provider != "ollama" || got.MaxToolIterations != 5 {
		t.Errorf("unset fields must keep base values: %+v", got)
	}
}

func TestLoadEnvExpansion(t *testing.T) {
	t.Setenv("WF_TEST_KEY", "sekret")
	dir := writeWorkflow(t, "name: e\nconfig:\n  api_key: ${WF_TEST_KEY}\nvars:\n  g: pre-${WF_TEST_KEY}-post\nsteps:\n  - name: a\n    prompt: x\n")
	wf, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wf.Config.APIKey == nil || *wf.Config.APIKey != "sekret" {
		t.Errorf("api_key env expansion: got %v", wf.Config.APIKey)
	}
	if wf.Vars["g"] != "pre-sekret-post" {
		t.Errorf("vars env expansion: got %q", wf.Vars["g"])
	}
}

func TestLoadValidationErrors(t *testing.T) {
	cases := map[string]string{
		"no steps and no main.lua": "name: x\n",
		"step missing name":        "name: x\nsteps:\n  - prompt: hi\n",
		"duplicate step name":      "name: x\nsteps:\n  - name: a\n    prompt: 1\n  - name: a\n    prompt: 2\n",
		"both prompt sources":      "name: x\nsteps:\n  - name: a\n    prompt: 1\n    prompt_file: p.txt\n",
		"no prompt source":         "name: x\nsteps:\n  - name: a\n",
		"bad on_error":             "name: x\nsteps:\n  - name: a\n    prompt: 1\n    on_error: explode\n",
		"unknown yaml key":         "name: x\nbogus: 1\nsteps:\n  - name: a\n    prompt: 1\n",
		"bad tools selector":       "name: x\nsteps:\n  - name: a\n    prompt: 1\n    tools: maybe\n",
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			if _, err := Load(writeWorkflow(t, body)); err == nil {
				t.Fatalf("expected error for %s", label)
			}
		})
	}
}

func TestResolvePathTraversal(t *testing.T) {
	wf, err := Load(writeWorkflow(t, "name: x\nsteps:\n  - name: a\n    prompt: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := wf.ResolvePath("../outside.png"); err == nil {
		t.Error("expected traversal to be rejected")
	}
	full, err := wf.ResolvePath("assets/a.png")
	if err != nil {
		t.Fatalf("valid relative path rejected: %v", err)
	}
	if !strings.HasPrefix(full, wf.Dir+string(filepath.Separator)) {
		t.Errorf("resolved path %q not under workflow dir %q", full, wf.Dir)
	}
}

func TestRenderTemplating(t *testing.T) {
	wf, err := Load(writeWorkflow(t, `
name: t
vars:
  topic: world
steps:
  - name: a
    prompt: 1
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := &Runner{wf: wf, stepOutputs: map[string]string{"prev": "PREVIOUS"}}
	got := r.render("hello {{topic}} / {{prev}} / {{steps.prev}} / {{unknown}} {{ }}")
	want := "hello world / PREVIOUS / PREVIOUS / {{unknown}} {{ }}"
	if got != want {
		t.Errorf("render:\n got %q\nwant %q", got, want)
	}
}
