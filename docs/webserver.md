# Web Server Mode

Run the harness as a browser-accessible web app instead of the terminal REPL:
live chat, multi-session management, tool/permission monitoring, MCP status,
persistent memory, transcript log tailing, and full provider/settings editing —
all without restarting the process.

```
go run main.go --server 0.0.0.0 --port 12345
```

`--server` takes the bind address (`0.0.0.0` for all interfaces, a specific
address like `192.168.1.123`, or `127.0.0.1` for local only). Either `--server`
or an explicit `--port` switches the process into web mode; without them the
REPL runs exactly as before. `--workflow` still short-circuits everything.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--server <addr>` | `""` (off) | Bind address for the web UI. Non-empty enables web mode. |
| `--port <n>` | `8080` | Web port. Setting it explicitly also enables web mode. |
| `--web-user <name>` | `""` | Login username. |
| `--web-pass <pass>` | `""` | Login password. |

Startup prints the same ready banner as the REPL plus
`Web UI listening on <addr>:<port>`. MCP tool servers, custom Lua tools, tool
permission policies, and persistent memory are initialized identically to the
REPL path (both call the shared `setupAgent()` bootstrap in [`main.go`](../main.go)).

## Authentication

Username/password login with a session cookie. Credential precedence:

1. `--web-user` / `--web-pass` flags
2. `HARNESS_WEB_USER` / `HARNESS_WEB_PASS` environment variables
3. `admin` / `admin` (a stderr warning names both sources when this fallback is used;
   binding a non-loopback address with the default password prints a stronger warning)

`/login` serves the login form. A successful login issues a `crypto/rand` token
stored server-side and sets the `harness_session` cookie (`HttpOnly`,
`SameSite=Lax`, `Path=/`, 24-hour expiry). Everything except `/login`,
`/logout`, `/static/*`, and `/favicon.ico` requires the cookie: unauthenticated
`/api/*` calls get `401 {"error":"not authenticated"}`, page requests redirect
to `/login`. `POST /logout` deletes the session server-side. Tokens expire
after 24 hours and are checked on every request.

> The web server exposes tools including `run_shell_command`. Treat the
> credential as a real secret on any non-loopback bind, and prefer TLS/reverse
> proxy in front of it if the UI crosses an untrusted network — the server
> itself speaks plain HTTP.

## UI tour

Single page, no build step, embedded in the binary via `go:embed`
([`web/static/`](../web/static)). Header bar shows the model/provider chip, a
context-% meter (from the last turn's prompt tokens vs the model's context
window), tool/MCP counts, and the memory file.

- **Chat** — transcript with streaming replies: reasoning traces in a collapsible
  "thinking" block, tool calls as collapsible cards (arguments, result,
  ok/failed), and **approval cards** for gated tools (see below). The composer
  supports Ctrl+Enter, image attachments (uploaded via `/api/upload`, sent as
  image parts — `@path` tokens in the text also work exactly like the REPL),
  and a Stop button that cancels the in-flight turn. Per-session actions:
  reset, compact, save, load.
- **Sessions** — sidebar list; create/rename/delete/switch. Sessions share one
  model client and one tool registry but keep independent histories. Saving
  writes `agent/sessions/<slug>.json` (same format as the REPL's `/save`).
- **Tools** — every registered tool with its policy (`allow`/`ask`/`deny`);
  change policies live from the dropdown, and reload custom Lua tools + config
  policies with one button.
- **MCP** — configured MCP servers, whether each is running, and the tools each
  contributed.
- **Memory** — the persistent memory file's entries (newest last, like `/memory`)
  plus an add-entry box.
- **Logs** — browse and tail the JSONL transcript files from roadmap 3.1
  (`log-dir` in `config.yaml`). Parsed per-line summaries (request / response /
  tool records) with the raw JSON behind each, plus a Follow toggle for live
  tailing.
- **Settings** — edit provider, base URL, model, API key, system prompt,
  temperature, context window, iteration/parallelism/retry caps, request
  timeout, compaction, streaming, and log dir. "Test connection" probes the
  entered endpoint without saving.

## Tool approvals in the browser

Tools whose policy is `ask` (by default `run_shell_command`) pause the agent
turn: the chat shows an Approve/Deny card with the tool name and arguments.
Approving runs the tool; denying feeds the same "user declined" result to the
model as the REPL's y/N prompt. If nobody answers within **2 minutes** the
request auto-denies (shown as "timed out — auto-denied"), so an abandoned tab
can never wedge a turn.

## Settings persistence

Settings changed in the browser are layered on top of the Go config preset and
saved atomically to `agent/web-settings.json` (created on first save). On the
next start they load automatically, so the preset in [`harness/config.go`](../harness/config.go)
remains the fallback and `config.yaml` stays untouched. The convention is
"zero value = no override": blank fields fall back to the preset. The API key
is never sent back to the browser in the clear — `GET /api/settings` returns it
masked (`****last4`), and saving that mask back means "keep the existing key".

A successful settings save **reconfigures live sessions in place** (new client,
new config, memory re-injected, history preserved) via
`Harness.Reconfigure` — no restart needed. One exception: transcript logging is
fixed at harness construction, so changing `log-dir` applies to sessions
created afterwards.

## API reference

All routes below require the session cookie. JSON request/response unless noted.
Implementations in [`web/server.go`](../web/server.go).

| Route | Purpose |
|---|---|
| `GET /` | The SPA shell |
| `GET /api/status` | Model, provider, base URL, context tokens/%, sessions, counts, memory file, log dir |
| `GET /api/sessions` | `{sessions, active}` |
| `POST /api/sessions` `{name}` | Create session → `201 {session}` |
| `GET /api/sessions/{id}` | `{session, history:[MessageView]}` |
| `DELETE /api/sessions/{id}` | Delete (refuses the last session) |
| `POST /api/sessions/{id}/switch` / `rename` | Activate / rename |
| `POST /api/sessions/{id}/chat` `{text, images}` | Start a turn → `202`; output arrives on the events stream |
| `GET /api/sessions/{id}/events` | SSE stream of turn events (see below) |
| `POST /api/sessions/{id}/approve` `{call_id, approved}` | Answer an approval card |
| `POST /api/sessions/{id}/cancel` | Cancel the in-flight turn |
| `POST /api/sessions/{id}/reset` / `compact` | REPL equivalents (`/reset`, `/compact`) |
| `POST /api/sessions/{id}/save` / `load {path}` | REPL `/save` `/load` equivalents |
| `GET /api/tools` | `[{name, policy}]` |
| `PUT /api/tools/policy` `{name, policy}` | Live policy change |
| `POST /api/tools/reload` | Lua hot-reload + policy reapply |
| `GET /api/mcp` | Server status + contributed tools |
| `GET /api/memory` / `POST /api/memory {text}` | View / append memory entries |
| `GET /api/logs` | Transcript file list (or `disabled` + reason) |
| `GET /api/logs/{name}?offset=N&limit=M` | Parsed, paginated transcript page |
| `GET /api/logs/{name}/follow` | SSE tail of a transcript |
| `GET` / `PUT /api/settings` | Read (key masked) / save settings |
| `POST /api/settings/test` | Probe an endpoint → `{ok, latency_ms, context_length?, error?}` |
| `POST /api/upload` | Multipart `file` (png/jpg/jpeg/gif/webp, 20 MB cap) → `{path}` |

### Event stream

`GET /api/sessions/{id}/events` emits `data: <json>\n\n` frames with a `type`:

`hello` (stream opened), `status` (turn running / compacting), `thinking`
(reasoning delta), `content` (reply delta), `tool_call` / `tool_result`
(completed tool activity with args/result/ok), `approval_request` (gated tool
pending — carries the call `id`), `approval_resolved`
(`approved|denied|timeout|cancelled`), `usage` (token counts), `error`,
`done` (turn finished).

The queue is best-effort (a slow consumer can miss events), so the UI
re-fetches full history on every `done` — treat the stream as live painting and
the history endpoint as the truth.

## Testing web mode

Unit tests: `go test ./web/...` (auth, settings, sessions incl. approval
grant/timeout, logs, router, static assets). Note the repo-wide test-cwd
caveat: run with `AGENT_CONFIG_PATH=<repo>\agent` (or compile the test binary
and run it from the repo root) because `harness/config`'s viper init resolves
`config.yaml` relative to the test's working directory.

## Known limitations

- Chat turns are serialized per session (one turn at a time per session), and
  tool-policy changes while a turn is in flight are serialized but not atomic.
- Tool call/result events are derived from history diffs, so they show
  completed tool activity at each tool round, not mid-execution progress.
- Log following polls the file every 500 ms.
- MCP status resolves server→tool attribution by name prefix only when the
  configured server list and running client list can't be paired positionally.
- The login page and cookies are plain HTTP — put a TLS-terminating proxy in
  front for untrusted networks.
