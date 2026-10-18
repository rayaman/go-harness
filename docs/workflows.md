# Workflows

A workflow is a directory you can run instead of the interactive REPL:

```
go run main.go --workflow path/to/my_workflow
go run main.go --workflow workflows/models-report -- any extra args go to harness.args
go run main.go --workflow path/to/my_workflow --base-config openai
```

The process exits `0` on success, `1` on the first fatal step error. The
REPL is skipped entirely when `--workflow` is present.

## Directory layout

```
my_workflow/
├── workflow.yaml   # required: config overrides + vars + steps
├── main.lua        # optional: programmatic mode (replaces the steps loop)
├── prompts/        # optional: prompt_file targets (your naming, any files)
├── assets/         # optional: media attachments (png/jpg/jpeg/gif/webp)
├── scripts/        # optional: per-step pre/post Lua scripts
└── tools/          # optional: per-workflow Lua tools (auto-detected)
```

Sample workflows: [`workflows/echo-demo/`](../workflows/echo-demo/workflow.yaml)
(declarative steps) and [`workflows/models-report/`](../workflows/models-report/main.lua)
(programmatic Lua).

## workflow.yaml

```yaml
name: my_workflow            # optional, defaults to the directory name
description: what it does    # optional
work_dir: .                  # optional: BaseDir for builtin file tools

vars:                        # string values usable as {{name}} in prompts
  topic: "quarterly earnings"

config:                      # partial overrides merged onto the base preset
  provider: ollama           # "ollama" (native /api/chat, images, num_ctx)
  base_url: http://localhost:11434/v1
  model: llama3.2-vision
  api_key: ${MY_API_KEY}     # ${ENV_VAR} expansion - keep secrets out of YAML
  system_prompt: |
    You are ...
  temperature: 0.3
  max_tool_iterations: 25
  max_context_tokens: 0      # 0 = auto-probe the server for the real window
  request_timeout: 10m       # per-request HTTP timeout (Go duration: "10m", "300s"); default 5m
  image_max_dimension: 2048  # downscale attached images to this longest-side pixel cap
                             # (0/absent = send images exactly as on disk; aspect ratio
                             # is always preserved and images are never upscaled)
  stream_thinking: true      # workflow-level bool, default false: stream the model's
                             # thinking/reasoning output live to the terminal (printed
                             # as "[thinking] ..." lines; step results print as normal)

tools:
  builtin: [current_time, read_file, list_files]  # names, or [all], or []
  lua_dir: tools             # .lua files loaded as model tools (default: auto-detect tools/)

steps:
  - name: summarize          # unique; output recorded as steps.summarize
    prompt: "Summarize {{topic}}"   # inline prompt, OR:
    # prompt_file: prompts/summarize.txt
    system_prompt: "optional persona - starts a FRESH conversation"
    model: "other-model"            # optional per-step model override
    temperature: 0.1
    media: [assets/chart.png]       # images relative to the workflow dir
    tools: none                     # none | all | [names] - per-step filter
    pre_script: scripts/pre.lua     # runs before the model call
    post_script: scripts/post.lua   # runs after; may rewrite `result`
    save_as: summary                # extra alias in steps.*
    on_error: stop                  # stop (default) | skip

  - name: report
    prompt_file: prompts/report.txt # may reference {{steps.summarize}}
```

Unknown YAML keys are rejected at load time, so typos fail loudly.

### Templating

Inline prompts and `prompt_file` contents run through a simple placeholder
pass before being sent:

- `{{var_name}}` / `{{vars.var_name}}` → workflow `vars`
- `{{step_name}}` / `{{steps.step_name}}` → output of an earlier step
- unknown placeholders are left as-is (literal braces are safe)

### Execution model

- `main.lua` present → it drives everything; declare zero steps or call
  them from Lua with `harness.run_step("name")`.
- Otherwise steps run in order. One shared conversation carries history
  across steps; a step with `system_prompt` (or Lua `ask` with
  `system`/`reset`) starts a fresh conversation instead.
- `media` files must be supported images (png/jpg/jpeg/gif/webp) inside
  the workflow directory; they're attached via the same provider-aware
  path the REPL's `@file` syntax uses (Ollama native `images:`, OpenAI
  multimodal `image_url` parts).

## Lua API (main.lua, and `require("harness")` in step scripts)

The VM also ships the sandboxed `http` and `filesystem` modules from
[`harness/lua-tool`](../harness/lua-tool/README.md) (filesystem BaseDir =
the workflow's `work_dir`). Both are reached with
`local fs = require("filesystem")` and they reject paths that escape
`work_dir` - use `harness.external` below for human-supplied paths.

| Call | Returns |
|---|---|
| `harness.ask(prompt)` / `harness.ask(prompt, opts)` | `text, err` |
| `harness.ask_media(prompt, {"a.png"}, opts?)` | `text, err` |
| `harness.run_step("name")` | `text, err` (YAML step) |
| `harness.list_models()` | `{{id=, modified_at=, size=, size_human=}, ...}, err` |
| `harness.context_length(model?)` | `tokens, err` |
| `harness.set_system_prompt(s)` | resets conversation to new persona |
| `harness.set_model(m)` | changes the default model |
| `harness.step_results()` | `{name = output, ...}` |
| `harness.vars` / `harness.args` | workflow vars / CLI args after `--` |
| `harness.log(...)` | stdout |
| `harness.external.abspath(p)` | absolute path, err (relative to CWD) |
| `harness.external.exists(p)` / `.is_dir(p)` | `true/false, err` (missing path = `false, nil`) |
| `harness.external.mkdir(p)` | created dir, err (`os.MkdirAll`) |
| `harness.external.list_images(dir)` | sorted absolute paths of png/jpg/jpeg/gif/webp files, err (recursive) |
| `harness.external.read_file(p)` | `text, err` |
| `harness.external.write_file(p, content)` | `message, err` (parent dirs auto-created) |
| `json.encode(t)` / `json.decode(s)` | table ⇄ JSON string |

`harness.external.*` deliberately bypasses the workflow-dir sandbox. The
safety model is provenance: these functions only exist inside the
`main.lua` VM, never in the model's tool registry, so they can only ever
touch paths the *human* supplied (prompt answers, CLI args) - main.lua
must keep model-generated text out of those paths. For images,
`harness.ask_media`/`media:` also accept absolute paths outside the
workflow directory.

`opts` table keys: `model`, `temperature`, `media = {paths}`, `system`
(fresh conversation with this persona), `reset = true`, `tools = "none" |
"all" | {names}`.

Step `pre_script`/`post_script` files see globals `step = {name, prompt,
media, vars, steps}` and `log(...)`; pre scripts may rewrite `step.prompt`
/ `step.media`, post scripts may reassign `result`. They can
`require("http")` and `require("filesystem")`.

## Adding tools for workflows

Per-workflow Lua tools go in `<workflow>/tools/*.lua` using the existing
tool contract (`name()`, `description()`, `parameters()`, `execute(args)`)
— see [docs/custom_tools.md](custom_tools.md). Builtin Go tools are
selected by name in `tools.builtin`; available names: `current_time`,
`read_file`, `list_files`, `read_file_partial`, `get_file_size`,
`edit_file`, `write_file`.

## Sample: manga-translator

[`workflows/manga-translator/`](../workflows/manga-translator/) translates
a directory of manga/comic page images, writing one `<page>.md` per image
(Technical Translation Framework: narrative context, spatial position,
speaker + psychological state, dual-language lines, SFX treatment) plus a
running `summary.md` that is fed into every later page's prompt. Its
system prompt explicitly authorizes faithful translation of fictional
adult (18+) source material. It prompts for the input/output directories
(or takes them as args) and uses `harness.external` for all out-of-dir IO:

```
go run main.go --workflow workflows/manga-translator
go run main.go --workflow workflows/manga-translator -- "C:\manga\ch01" "C:\manga\out" Japanese English
```

Re-running skips pages whose `.md` already exists in the output dir
(delete a page's `.md` to redo just that page).

## Tests

`harness/workflow` carries unit + integration tests that run against an
in-process fake chat server (no model required):

```
go test ./harness/workflow/
```

Note: this repo's `harness/config` package panics at init without a
`config.yaml`, so test runs need `AGENT_CONFIG_PATH` pointed at a config
directory (e.g. `set AGENT_CONFIG_PATH=z:\harness-go\agent`).
