// -----------------------------------------------------------------------
// A note on adding more powerful tools (shell exec, network calls, writes,
// installing packages, etc.): treat anything that executes commands or
// mutates state as dangerous by default, since the MODEL - not you -
// decides when to call it mid-conversation. If you add a tool like that,
// strongly consider:
//   - a human confirmation step before it actually runs (easy to add in
//     Harness.runTool in harness.go)
//   - an allowlist of permitted commands/paths
//   - running it inside a sandbox or container, not directly on the host
// -----------------------------------------------------------------------

## Ollama provider and image support

### Choosing a provider (`Config.Provider`)

`Config.Provider` selects which request path the harness takes:

- `"ollama"` — uses Ollama's native `/api/chat` endpoint.
- anything else (including the empty string) — uses the OpenAI-compatible
  `/v1/chat/completions` path, same as before.

There is a preset for the native path in `harness/config.go`:

```go
cfg := harness.OllamaConfig()
// BaseURL:          "http://localhost:11434/v1"
// Provider:         "ollama"
// Model:            "llama3.2-vision"
// MaxContextTokens: 32768
```

Note: there is currently no CLI flag for provider selection. `main.go`
hardcodes `cfg := harness.BdoConfig()`. To switch to Ollama, edit that
line to `cfg := harness.OllamaConfig()`, or override the fields after
preset construction — the same pattern already shown in the commented
manual-override lines in `main.go`:

```go
cfg := harness.BdoConfig()
cfg.Provider = "ollama"
cfg.BaseURL = "http://localhost:11434/v1"
cfg.Model = "llama3.2-vision"
```

Keep the `/v1` suffix on `BaseURL`: the native client derives `/api/chat`
by stripping the trailing `/v1`, so one setting works for both paths.

### num_ctx (`MaxContextTokens`)

For provider `"ollama"`, `Config.MaxContextTokens` is sent to Ollama as
`options.num_ctx` — but only when it is greater than 0. Zero or negative
leaves the server default in place.

Heads-up: vision models are token-hungry. Images consume a lot of context,
so raising `MaxContextTokens` (e.g. 32768, as the preset does) is often
needed for image prompts to fit at all.

### Attaching images from the REPL (`@file` syntax)

On the REPL input line, any whitespace-delimited token that starts with
`@` is treated as an image path: the `@` is stripped, the token is removed
from the text sent to the model, and the path is attached to the request.

```
> what's in this picture? @C:\pics\cat.png
> compare @./img/shot.jpg and describe the scene
> @"C:\pics\cat.png" describe this
```

Details:

- A single pair of matching surrounding quotes is stripped from the path
  (e.g. `@"C:\pics\cat.png"`), but tokens are split on whitespace first,
  so a path containing SPACES cannot work — it would never be one token.
- A bare `@` with nothing after it is left as literal text.
- Supported formats (detected from the extension, case-insensitive):
  `png`, `jpg`, `jpeg`, `gif`, `webp`. Anything else is rejected.
- Size guard: 20 MB max per image.
- Per-file errors (unsupported extension, unreadable file, too large) are
  printed to stderr as `warning: skipping image: ...`. The remaining
  images plus the prompt still go through — one bad path never aborts
  the prompt.
- The wire format is provider-appropriate automatically; you don't
  choose it. Ollama native gets raw base64 in `message.images`;
  OpenAI-compatible gets `content` parts with `image_url` data URIs
  (`data:<mime>;base64,...`).

### Vision models

Use a vision-capable Ollama model for image prompts, e.g. `llava` or
`qwen2.5vl`. The preset defaults to `llama3.2-vision`; change it via
`cfg.Model`. Non-vision models will reject or ignore image payloads.

### Quick API reference

```go
reply, err := h.Ask(ctx, "text only")
reply, err := h.AskWithImages(ctx, "what's in this?", []string{`C:\pics\cat.png`})
```

`Ask` is text-only. `AskWithImages` takes image paths (same limits and
per-file warning behavior as the REPL `@file` syntax) and encodes them
itself.