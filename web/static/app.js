// ai-harness SPA — vanilla ES2020, no frameworks. Sections:
// helpers · api · toasts · status bar · sessions sidebar · tabs ·
// chat + session SSE · tools · MCP · memory · logs · settings · boot.
"use strict";

// ---------------------------------------------------------------------------
// Tiny DOM helper. el(tag, opts, kids): opts.class/.text/.title/.type/
// .value/.hidden/.onclick/.onchange; a bare string is shorthand for class.
// Every rendered string goes through textContent/createTextNode — tool
// args, model output and log lines are untrusted, no innerHTML anywhere.
// ---------------------------------------------------------------------------
function el(tag, opts = {}, kids = []) {
  const n = document.createElement(tag);
  const o = typeof opts === "string" ? { class: opts } : opts;
  if (o.class) n.className = o.class;
  if (o.text != null) n.textContent = o.text;
  if (o.title) n.title = o.title;
  if (o.type) n.type = o.type;
  if (o.value != null) n.value = o.value;
  if (o.colspan) n.colSpan = o.colspan;
  if (o.hidden) n.hidden = true;
  if (o.onclick) n.addEventListener("click", o.onclick);
  if (o.onchange) n.addEventListener("change", o.onchange);
  for (const k of kids) if (k) n.append(k);
  return n;
}
const $ = (id) => document.getElementById(id);

function fmtBytes(n) {
  if (n == null) return "";
  const u = ["B", "KB", "MB", "GB"];
  let v = n, i = 0;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i ? v.toFixed(1) : String(v)) + " " + u[i];
}
function fmtDate(iso) {
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "" : d.toLocaleString(undefined, { hour12: false });
}
function prettyJSON(s) {
  try { return JSON.stringify(JSON.parse(s), null, 2); } catch (_) { return s; }
}

// ---------------------------------------------------------------------------
// api(): one fetch wrapper. JSON headers unless a FormData body is given
// (upload needs the browser's multipart boundary). Non-2xx throws with the
// server's {"error": "..."} message; a 401 bounces to the login page the
// server owns.
// ---------------------------------------------------------------------------
async function api(path, opts = {}) {
  const init = { method: opts.method || "GET", credentials: "same-origin", headers: {} };
  if (opts.json !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(opts.json);
  } else if (opts.form !== undefined) {
    init.body = opts.form;
  }
  const res = await fetch(path, init);
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`;
    try { const j = await res.json(); if (j && j.error) msg = j.error; } catch (_) { /* non-JSON body */ }
    if (res.status === 401) location.assign("/login");
    const e = new Error(msg);
    e.status = res.status;
    throw e;
  }
  return res.status === 204 ? null : res.json();
}

function toast(msg, kind = "") {
  const t = el("div", { class: "toast" + (kind ? " " + kind : ""), text: String(msg) });
  t.addEventListener("click", () => t.remove());
  $("toasts").append(t);
  setTimeout(() => t.remove(), 8000);
}

// ---------------------------------------------------------------------------
// App state — the only global beyond the helpers above.
// ---------------------------------------------------------------------------
const state = {
  status: null,          // last GET /api/status body
  activeId: null,        // active session id
  es: null,              // the ONE open session EventSource
  turnActive: false,
  compacting: false,     // set between POST compact and its done event
  stream: { thinking: null, content: null }, // live streaming blocks
  toolCards: new Map(),  // tool_call id -> card refs
  approvals: new Map(),  // pending approvals: id -> {card, cd, tick}
  attachments: [],       // uploaded image paths riding the next chat body
  tab: "chat",
  logs: { name: "", offset: 0, hasMore: false, es: null, stick: true, wanted: false },
};

// ---------------------------------------------------------------------------
// Status bar
// ---------------------------------------------------------------------------
async function refreshStatus() {
  try { state.status = await api("/api/status"); }
  catch (e) { toast(e.message, "err"); return; }
  const s = state.status;
  $("chip-model").textContent = `${s.model} · ${s.provider}`;
  $("chip-model").title = s.base_url || "";
  $("chip-tools").textContent = `${s.tools_count} tools · ${s.mcp_servers_count} mcp`;
  $("chip-memory").textContent = s.memory_file ? `mem: ${s.memory_file}` : "memory off";
  renderMeter(s.prompt_tokens, s.context_tokens, s.context_percent);
  renderSessions(s.sessions || []);
}
function renderMeter(prompt, ctx, pct) {
  const p = Math.max(0, Math.min(100, pct || 0));
  const f = $("ctx-fill");
  f.style.width = p.toFixed(1) + "%";
  f.classList.toggle("hot", p >= 85);
  $("ctx-label").textContent = ctx > 0
    ? `${prompt} / ${ctx} tok (${p.toFixed(0)}%)`
    : "no context info";
}

// ---------------------------------------------------------------------------
// Sessions sidebar
// ---------------------------------------------------------------------------
function renderSessions(list) {
  const ul = $("session-list");
  ul.replaceChildren();
  for (const s of list) {
    const li = el("li", { class: s.active ? "active" : "" }, [
      el("span", { class: "s-name", text: s.name }),
      el("span", { class: "s-meta", text: `${s.messages} msgs · ${fmtDate(s.created_at)}` }),
    ]);
    li.addEventListener("click", () => switchSession(s.id));
    ul.append(li);
  }
}

async function switchSession(id) {
  if (id === state.activeId) return;
  try { await api(`/api/sessions/${id}/switch`, { method: "POST", json: {} }); }
  catch (e) { toast(e.message, "err"); return; }
  state.activeId = id;
  attachSessionStream(); // close the old one, open the new session's
  await loadHistory();   // and load that session's authoritative history
  refreshStatus();
}

async function newSession() {
  const name = window.prompt("New session name (blank = auto-numbered):") ?? "";
  try {
    const r = await api("/api/sessions", { method: "POST", json: { name } });
    state.activeId = r.session.id; // Create makes it active server-side
    attachSessionStream();
    await loadHistory();
    refreshStatus();
  } catch (e) { toast(e.message, "err"); }
}

async function renameSession() {
  if (!state.activeId) return;
  const cur = (state.status?.sessions.find((s) => s.id === state.activeId) || {}).name || "";
  const name = window.prompt("Rename session:", cur);
  if (name == null) return;
  try {
    await api(`/api/sessions/${state.activeId}/rename`, { method: "POST", json: { name } });
    refreshStatus();
  } catch (e) { toast(e.message, "err"); }
}

async function deleteSession() {
  if (!state.activeId) return;
  if (!window.confirm("Delete the active session? Its saved file stays on disk.")) return;
  try { await api(`/api/sessions/${state.activeId}`, { method: "DELETE" }); }
  catch (e) { toast(e.message, "err"); return; }
  // The server moved the active pointer to the first remaining session.
  try {
    const l = await api("/api/sessions");
    state.activeId = l.active;
    attachSessionStream();
    await loadHistory();
    refreshStatus();
  } catch (e) { toast(e.message, "err"); }
}

// Shared by the sidebar and the Chat tab bar.
async function sessionAction(kind) {
  const id = state.activeId;
  if (!id) return;
  try {
    if (kind === "reset") {
      await api(`/api/sessions/${id}/reset`, { method: "POST", json: {} });
      resetLive();
      renderHistory([]);
      toast("session reset", "ok");
    } else if (kind === "compact") {
      await api(`/api/sessions/${id}/compact`, { method: "POST", json: {} }); // 202
      state.compacting = true; // progress rides the SSE: status → done/error
      setTurnActive(true);
      toast("compaction started — watch the transcript");
    } else if (kind === "save") {
      const r = await api(`/api/sessions/${id}/save`, { method: "POST", json: {} });
      toast("saved: " + r.path, "ok");
    } else if (kind === "load") {
      const p = window.prompt("Path of the saved session JSON to load:");
      if (!p) return;
      await api(`/api/sessions/${id}/load`, { method: "POST", json: { path: p } });
      await loadHistory();
      refreshStatus();
      toast("loaded " + p, "ok");
    }
  } catch (e) { toast(e.message, "err"); }
}

// ---------------------------------------------------------------------------
// Tabs
// ---------------------------------------------------------------------------
const tabLoaders = {
  tools: loadTools, mcp: loadMCP, memory: loadMemory,
  logs: loadLogsList, settings: loadSettings,
};
function showTab(name) {
  state.tab = name;
  for (const b of document.querySelectorAll(".tab")) b.classList.toggle("active", b.dataset.tab === name);
  for (const p of document.querySelectorAll(".panel")) p.classList.toggle("active", p.id === "panel-" + name);
  $("chat-actions").hidden = name !== "chat";
  const f = tabLoaders[name];
  if (f) f().catch((e) => toast(e.message, "err"));
}

// ---------------------------------------------------------------------------
// Chat: transcript rendering
// ---------------------------------------------------------------------------
const transcript = () => $("transcript");
function scrollTranscript() { const t = transcript(); t.scrollTop = t.scrollHeight; }

// MessageView -> role-styled bubble (+N badges, thinking disclosure).
function messageBubble(m) {
  const cls = ["user", "assistant", "system", "tool"].includes(m.role) ? m.role : "system";
  const wrap = el("div", { class: "msg " + cls });
  const label = Number.isInteger(m.index) && m.index >= 0 ? `${m.role} #${m.index}` : m.role;
  wrap.append(el("div", { class: "role", text: label }));
  wrap.append(el("div", { class: "body", text: m.text || "" }));
  const badges = [];
  if (m.tool_calls) badges.push(el("span", { class: "badge", text: `+${m.tool_calls} tool calls` }));
  if (m.images) badges.push(el("span", { class: "badge", text: `+${m.images} images` }));
  if (badges.length) wrap.append(el("div", { class: "badges" }, badges));
  if (m.reasoning) {
    wrap.append(el("details", {}, [
      el("summary", { text: "thinking" }),
      el("pre", { text: m.reasoning }),
    ]));
  }
  return wrap;
}
function renderHistory(history) {
  transcript().replaceChildren(...history.map(messageBubble));
  scrollTranscript();
}
async function loadHistory() {
  if (!state.activeId) return;
  try {
    const data = await api(`/api/sessions/${state.activeId}`);
    renderHistory(data.history || []);
  } catch (e) { toast(e.message, "err"); }
}

// ---------------------------------------------------------------------------
// Session SSE: exactly one open EventSource per active session. Switching
// sessions closes the old stream and opens the new one. EventSource can
// wedge after a dropped connection, so on error we close it deliberately
// and reopen after a short backoff (guarded against a session switch or a
// duplicate reopen landing in between).
// ---------------------------------------------------------------------------
function attachSessionStream() {
  if (state.es) { state.es.close(); state.es = null; }
  resetLive();
  const id = state.activeId;
  if (!id) return;
  const es = new EventSource(`/api/sessions/${id}/events`);
  state.es = es;
  es.onmessage = (msg) => {
    let ev;
    try { ev = JSON.parse(msg.data); } catch (_) { return; }
    handleSessionEvent(ev);
  };
  es.onerror = () => {
    if (state.es !== es) return; // superseded by a switch already
    es.close();
    state.es = null;
    setTimeout(() => {
      if (state.activeId === id && !state.es) attachSessionStream();
    }, 2000);
  };
}

function resetLive() {
  state.stream = { thinking: null, content: null };
  state.toolCards.clear();
  for (const a of state.approvals.values()) clearInterval(a.tick);
  state.approvals.clear();
}

function setTurnActive(on) {
  state.turnActive = on;
  $("btn-stop").disabled = !on;
}

function handleSessionEvent(ev) {
  switch (ev.type) {
    case "hello": // subscriber handshake: session id confirmed
      break;
    case "status":
      if (ev.message === "running") setTurnActive(true);
      else if (ev.message === "compacting") addStatusLine("compacting history…");
      break;
    case "thinking":
      appendStreamBlock("thinking", "stream-thinking", ev.text || "");
      break;
    case "content":
      appendStreamBlock("content", "stream-content", ev.text || "");
      break;
    case "tool_call":
      addToolCard(ev);
      break;
    case "tool_result":
      finishToolCard(ev);
      break;
    case "approval_request":
      addApprovalCard(ev);
      break;
    case "approval_resolved":
      resolveApprovalCard(ev);
      break;
    case "usage":
      if (ev.usage && state.status) {
        const ctx = state.status.context_tokens || 0;
        renderMeter(ev.usage.prompt_tokens, ctx,
          ctx ? (ev.usage.prompt_tokens / ctx) * 100 : 0);
      }
      break;
    case "error":
      addInline("inline-error", "error: " + (ev.message || "turn failed"));
      setTurnActive(false);
      reconcile(); // cancelled turns roll History back — resync
      break;
    case "done": {
      setTurnActive(false);
      if (state.compacting) {
        state.compacting = false;
        toast(ev.text || "compact finished", "ok");
      }
      // The event queue is lossy (slow consumers drop events), so the
      // live view is only optimistic: on done re-fetch the authoritative
      // history and replace everything rendered mid-turn.
      reconcile();
      break;
    }
    case "log":
      if (ev.message) addStatusLine(ev.message);
      break;
  }
}

function appendStreamBlock(role, cls, delta) {
  const key = role === "thinking" ? "thinking" : "content";
  if (!state.stream[key]) {
    const body = el("div", { class: cls + " caret" });
    const block = el("div", { class: "stream-block" }, [
      el("div", { class: "role", text: role }),
      body,
    ]);
    transcript().append(block);
    state.stream[key] = body;
  }
  state.stream[key].append(document.createTextNode(delta));
  scrollTranscript();
}

function addInline(cls, text) {
  transcript().append(el("div", { class: cls, text }));
  scrollTranscript();
}
const addInlineError = (m) => addInline("inline-error", m);
const addStatusLine = (m) => addInline("inline-status", m);

// Collapsible tool cards: args pretty-printed, result + ok flag when the
// tool_result event lands (they arrive in History order after the turn's
// tool round, per the server's derived-event design).
function addToolCard(ev) {
  const summary = el("summary", { text: "▸ " + (ev.name || "tool") });
  const card = el("details", { class: "tool-card" }, [
    summary,
    el("pre", { text: prettyJSON(ev.args || "") }),
  ]);
  const resultPre = el("pre", { hidden: true });
  card.append(resultPre);
  transcript().append(card);
  if (ev.id) state.toolCards.set(ev.id, { summary, resultPre });
  scrollTranscript();
}
function finishToolCard(ev) {
  const flag = el("span", { class: "ok-flag " + (ev.ok ? "ok" : "bad"), text: ev.ok ? " ok" : " failed" });
  const t = state.toolCards.get(ev.call_id);
  if (t) {
    t.resultPre.hidden = false;
    t.resultPre.textContent = ev.result || "(no result)";
    t.summary.append(flag);
  } else {
    // Orphan result (tool_call frame was dropped): render standalone.
    const summary = el("summary", { text: "▸ " + (ev.name || "tool") });
    summary.append(flag);
    transcript().append(el("details", { class: "tool-card" }, [
      summary, el("pre", { text: ev.result || "" }),
    ]));
  }
  scrollTranscript();
}

// Approvals: PolicyAsk tools block up to 2 minutes server-side, then the
// server auto-denies with resolved="timeout" — the countdown and the
// freeze on approval_resolved make that visible.
const APPROVAL_TIMEOUT_S = 120;
function addApprovalCard(ev) {
  const card = el("div", { class: "approval-card" });
  card.append(el("h4", { text: `Approve tool call: ${ev.name || "unknown"}` }));
  card.append(el("pre", { text: prettyJSON(ev.args || "") }));
  const cd = el("span", { class: "countdown" });
  const approve = el("button", { class: "btn approve", text: "Approve" });
  const deny = el("button", { class: "btn deny", text: "Deny" });
  approve.addEventListener("click", () => answerApproval(ev.id, true));
  deny.addEventListener("click", () => answerApproval(ev.id, false));
  card.append(el("div", { class: "row" }, [approve, deny, cd]));
  transcript().append(card);

  const deadline = Date.now() + APPROVAL_TIMEOUT_S * 1000;
  const tick = setInterval(() => {
    const left = Math.max(0, Math.round((deadline - Date.now()) / 1000));
    cd.textContent = left > 0 ? `auto-denies in ${left}s` : "awaiting server timeout…";
  }, 1000);
  tick();
  state.approvals.set(ev.id, { card, cd, tick });
  scrollTranscript();
}
async function answerApproval(callID, approved) {
  try {
    await api(`/api/sessions/${state.activeId}/approve`, { method: "POST", json: { call_id: callID, approved } });
  } catch (e) { toast(e.message, "err"); } // 404 = already answered/stale
}
function resolveApprovalCard(ev) {
  const label = {
    approved: "approved",
    denied: "denied",
    timeout: "timed out — auto-denied after 2 minutes",
    cancelled: "cancelled with the turn",
  }[ev.resolved] || ev.resolved || "resolved";
  const a = state.approvals.get(ev.id);
  if (!a) { addStatusLine(`approval ${label}`); return; }
  clearInterval(a.tick);
  state.approvals.delete(ev.id);
  a.card.classList.add("resolved");
  for (const b of a.card.querySelectorAll("button")) b.remove();
  a.cd.remove();
  a.card.append(el("div", { class: "outcome " + (ev.ok ? "ok" : "bad"), text: label }));
}

// Reconcile: replace the optimistic live view with the authoritative
// history (mandatory after every done, per the lossy-queue contract).
async function reconcile() {
  resetLive();
  await loadHistory();
  refreshStatus();
}

// ---------------------------------------------------------------------------
// Composer: Ctrl+Enter submits, attach uploads and chips the path, Stop
// cancels the in-flight turn. @path tokens in the text are passed through
// untouched — the backend expands them natively.
// ---------------------------------------------------------------------------
async function sendChat() {
  const text = $("chat-input").value.trim();
  const images = state.attachments.slice();
  if (!text && !images.length) return;
  try {
    await api(`/api/sessions/${state.activeId}/chat`, { method: "POST", json: { text, images } }); // 202
  } catch (e) {
    toast(e.message, "err");
    return; // keep the draft so a failed send can be retried
  }
  $("chat-input").value = "";
  clearAttachments();
  // Optimistic user bubble; reconcile() on done replaces the transcript
  // with the authoritative history anyway.
  transcript().append(messageBubble({ role: "user", index: -1, text }));
  scrollTranscript();
  setTurnActive(true);
}

function renderAttachments() {
  const host = $("attach-host");
  host.replaceChildren();
  state.attachments.forEach((p, i) => {
    host.append(el("span", { class: "attach-chip" }, [
      document.createTextNode(p),
      el("button", { text: "×", title: "remove attachment", onclick: () => { state.attachments.splice(i, 1); renderAttachments(); } }),
    ]));
  });
}
function clearAttachments() { state.attachments = []; renderAttachments(); }

// ---------------------------------------------------------------------------
// Tools tab
// ---------------------------------------------------------------------------
async function loadTools() {
  const r = await api("/api/tools");
  $("tools-count").textContent = `${r.tools.length} tools registered`;
  const body = $("tools-body");
  body.replaceChildren();
  for (const t of r.tools) {
    const sel = el("select");
    for (const p of ["allow", "ask", "deny"]) {
      const o = document.createElement("option");
      o.value = p; o.textContent = p;
      if (p === t.policy) o.selected = true;
      sel.append(o);
    }
    sel.dataset.prev = t.policy;
    sel.addEventListener("change", async () => {
      // PUT immediately; on failure toast and snap back.
      try {
        await api("/api/tools/policy", { method: "PUT", json: { name: t.name, policy: sel.value } });
        sel.dataset.prev = sel.value;
      } catch (e) {
        toast(e.message, "err");
        sel.value = sel.dataset.prev;
      }
    });
    body.append(el("tr", {}, [
      el("td", { class: "name", text: t.name }),
      el("td", {}, [sel]),
    ]));
  }
}

// ---------------------------------------------------------------------------
// MCP tab
// ---------------------------------------------------------------------------
async function loadMCP() {
  const r = await api("/api/mcp");
  const warn = $("mcp-warnings");
  warn.replaceChildren(...(r.warnings || []).map((w) => el("div", { class: "notice warn", text: w })));
  const body = $("mcp-body");
  body.replaceChildren();
  for (const s of r.servers || []) {
    body.append(el("tr", {}, [
      el("td", { class: "name", text: s.name }),
      el("td", { class: "mono", text: s.command }),
      el("td", {}, [el("span", { class: "pill " + (s.running ? "on" : "off"), text: s.running ? "running" : "stopped" })]),
      el("td", {}, [el("div", { class: "tool-tags" }, (s.tools || []).map((t) => el("span", { class: "badge", text: t })))]),
    ]));
  }
  if (!(r.servers || []).length) {
    body.append(el("tr", {}, [el("td", { colspan: 4, class: "muted", text: "no MCP servers configured" })]));
  }
}

// ---------------------------------------------------------------------------
// Memory tab
// ---------------------------------------------------------------------------
async function loadMemory() {
  const r = await api("/api/memory");
  $("memory-file").textContent = r.file ? `file: ${r.file}` : "memory disabled — set memory-file in config.yaml";
  const list = $("memory-list");
  list.replaceChildren();
  const entries = r.entries || [];
  if (!entries.length) list.append(el("li", { class: "muted", text: "(no entries)" }));
  for (const e of entries) list.append(el("li", { text: e })); // newest last, like /memory
}

// ---------------------------------------------------------------------------
// Logs tab: file pills, paged reading with an offset cursor, and a Follow
// toggle over the WRAPPED follow SSE (frames are {"type":"log","line":…}).
// ---------------------------------------------------------------------------
async function loadLogsList() {
  stopFollow();
  const r = await api("/api/logs");
  $("logs-dir").textContent = r.dir ? `dir: ${r.dir}` : "";
  const host = $("logs-files");
  host.replaceChildren();
  for (const f of r.files || []) {
    const pill = el("span", {
      class: "file-pill" + (f.name === state.logs.name ? " active" : ""),
      text: `${f.name} · ${fmtBytes(f.size)} · ${fmtDate(f.mod_time)}`,
    });
    pill.addEventListener("click", () => openLogFile(f.name));
    host.append(pill);
  }
  const d = $("logs-disabled");
  if (r.disabled) { d.hidden = false; d.textContent = r.reason || "logging disabled"; }
  else d.hidden = true;
  if (!state.logs.name && (r.files || []).length) openLogFile(r.files[0].name);
}

async function openLogFile(name) {
  stopFollow();
  state.logs = { name, offset: 0, hasMore: false, es: null, stick: true, wanted: false };
  $("log-host").replaceChildren();
  $("btn-log-more").hidden = true;
  for (const p of document.querySelectorAll("#logs-files .file-pill")) {
    p.classList.toggle("active", p.textContent.startsWith(name));
  }
  await fetchLogPage();
}

async function fetchLogPage() {
  const L = state.logs;
  if (!L.name) return;
  try {
    const page = await api(`/api/logs/${encodeURIComponent(L.name)}?offset=${L.offset}&limit=200`);
    appendLogLines(page.lines || []);
    L.offset = page.next_offset;
    L.hasMore = page.has_more;
    $("btn-log-more").hidden = !page.has_more || !!L.es;
  } catch (e) { toast(e.message, "err"); }
}

function appendLogLines(lines) {
  const host = $("log-host");
  for (const ln of lines) {
    const div = el("div", { class: "log-line kind-" + (ln.kind || "raw") });
    const sum = el("div", { class: "sum" });
    if (ln.ts) sum.append(el("span", { class: "ts", text: ln.ts }));
    sum.append(document.createTextNode(ln.summary || ""));
    div.append(sum);
    div.append(el("details", {}, [
      el("summary", { text: "raw" }),
      el("pre", { text: ln.raw || "" }),
    ]));
    host.append(div);
  }
  // Auto-scroll pinned to the bottom unless the user scrolled up.
  if (state.logs.stick) host.scrollTop = host.scrollHeight;
}

function setFollowBtn(on, label) {
  const b = $("btn-log-follow");
  b.classList.toggle("on", !!on);
  b.textContent = label || (on ? "Following" : "Follow");
}
function openFollow() {
  const L = state.logs;
  if (!L.name) { toast("select a transcript file first", "err"); return; }
  L.wanted = true;
  // Follow tails from the top of the file, so start the view fresh.
  $("log-host").replaceChildren();
  $("btn-log-more").hidden = true;
  const es = new EventSource(`/api/logs/${encodeURIComponent(L.name)}/follow`);
  L.es = es;
  setFollowBtn(true);
  es.onmessage = (msg) => {
    let frame;
    try { frame = JSON.parse(msg.data); } catch (_) { return; }
    if (frame.type === "log" && frame.line) appendLogLines([frame.line]); // unwrap .line
  };
  es.onerror = () => {
    if (L.es !== es) return;
    es.close();
    L.es = null;
    if (L.wanted && state.logs.name === L.name) {
      setFollowBtn(false, "reconnecting…");
      setTimeout(() => {
        if (state.logs.wanted && state.logs.name === L.name && !state.logs.es) openFollow();
      }, 2000);
    } else {
      setFollowBtn(false);
    }
  };
}
function stopFollow() {
  state.logs.wanted = false;
  if (state.logs.es) { state.logs.es.close(); state.logs.es = null; }
  setFollowBtn(false);
}

// ---------------------------------------------------------------------------
// Settings tab: one form over the effective (masked) settings. GET fills
// api_key with the **** sentinel; PUT echoes it back to mean "keep the
// stored key" — the placeholder says exactly that.
// ---------------------------------------------------------------------------
const SETTING_FIELDS = [
  "provider", "base_url", "api_key", "model", "system_prompt",
  "temperature", "max_context_tokens", "max_tool_iterations",
  "max_parallel_tools", "max_tool_output_bytes", "max_retries",
  "max_image_dimension", "request_timeout", "stream_thinking",
  "compact_threshold", "compact_keep_turns", "log_dir",
];
function fillSettingsForm(s) {
  for (const k of SETTING_FIELDS) {
    const input = $("set-" + k);
    if (!input) continue;
    if (input.type === "checkbox") input.checked = !!s[k];
    else input.value = s[k] == null ? "" : String(s[k]);
  }
}
function collectSettings() {
  const out = {};
  for (const k of SETTING_FIELDS) {
    const input = $("set-" + k);
    if (!input) continue;
    if (input.type === "checkbox") out[k] = input.checked;
    else if (input.type === "number") {
      const v = input.value.trim();
      if (v !== "") out[k] = Number(v); // blank = no override (preset wins)
    } else {
      const v = input.value;
      if (v !== "") out[k] = v;
    }
  }
  return out;
}
async function loadSettings() {
  fillSettingsForm(await api("/api/settings"));
}

// ---------------------------------------------------------------------------
// Wiring and boot
// ---------------------------------------------------------------------------
function wire() {
  for (const b of document.querySelectorAll(".tab")) {
    b.addEventListener("click", () => showTab(b.dataset.tab));
  }
  $("btn-logout").addEventListener("click", async () => {
    try { await api("/logout", { method: "POST", json: {} }); } catch (_) { /* expired session is fine */ }
    location.assign("/login");
  });

  $("btn-session-new").addEventListener("click", newSession);
  $("btn-session-rename").addEventListener("click", renameSession);
  $("btn-session-delete").addEventListener("click", deleteSession);
  $("btn-session-save").addEventListener("click", () => sessionAction("save"));
  $("btn-session-load").addEventListener("click", () => sessionAction("load"));
  $("btn-reset").addEventListener("click", () => sessionAction("reset"));
  $("btn-compact").addEventListener("click", () => sessionAction("compact"));
  $("btn-chat-save").addEventListener("click", () => sessionAction("save"));
  $("btn-chat-load").addEventListener("click", () => sessionAction("load"));

  $("btn-send").addEventListener("click", sendChat);
  $("chat-input").addEventListener("keydown", (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === "Enter") { e.preventDefault(); sendChat(); }
  });
  $("btn-attach").addEventListener("click", () => $("file-input").click());
  $("file-input").addEventListener("change", async () => {
    const f = $("file-input").files[0];
    $("file-input").value = "";
    if (!f) return;
    const form = new FormData();
    form.append("file", f);
    try {
      const r = await api("/api/upload", { method: "POST", form });
      state.attachments.push(r.path);
      renderAttachments();
    } catch (e) { toast(e.message, "err"); }
  });
  $("btn-stop").addEventListener("click", async () => {
    if (!state.activeId) return;
    try {
      const r = await api(`/api/sessions/${state.activeId}/cancel`, { method: "POST", json: {} });
      if (!r.ok) toast("no turn in flight");
    } catch (e) { toast(e.message, "err"); }
  });

  $("btn-tools-reload").addEventListener("click", async () => {
    try {
      await api("/api/tools/reload", { method: "POST", json: {} });
      toast("tools reloaded (config.yaml policies re-applied)", "ok");
      loadTools();
      refreshStatus();
    } catch (e) { toast(e.message, "err"); }
  });

  $("btn-memory-add").addEventListener("click", async () => {
    const text = $("memory-input").value.trim();
    if (!text) return;
    try {
      const r = await api("/api/memory", { method: "POST", json: { text } });
      $("memory-input").value = "";
      toast(r.written ? `entry added (${r.count} total)` : "duplicate entry — not written", r.written ? "ok" : "");
      loadMemory();
    } catch (e) { toast(e.message, "err"); }
  });

  $("btn-logs-refresh").addEventListener("click", () => loadLogsList().catch((e) => toast(e.message, "err")));
  $("btn-log-more").addEventListener("click", fetchLogPage);
  $("btn-log-follow").addEventListener("click", () => {
    if (state.logs.es || state.logs.wanted) stopFollow();
    else openFollow();
  });
  $("log-host").addEventListener("scroll", () => {
    const h = $("log-host");
    state.logs.stick = h.scrollHeight - h.scrollTop - h.clientHeight < 40;
  });

  $("btn-settings-save").addEventListener("click", async () => {
    try {
      const updated = await api("/api/settings", { method: "PUT", json: collectSettings() });
      fillSettingsForm(updated);
      toast("settings saved — pushed live through every session", "ok");
      refreshStatus();
    } catch (e) { toast(e.message, "err"); }
  });
  $("btn-settings-test").addEventListener("click", async () => {
    const out = $("test-result");
    out.textContent = "testing…";
    out.className = "muted";
    try {
      const r = await api("/api/settings/test", { method: "POST", json: collectSettings() });
      if (r.ok) {
        out.textContent = `ok — ${r.latency_ms} ms, context length ${r.context_length}`;
        out.className = "ok";
      } else {
        out.textContent = `failed — ${r.latency_ms} ms: ${r.error}`;
        out.className = "bad";
      }
    } catch (e) {
      out.textContent = "probe error: " + e.message;
      out.className = "bad";
    }
  });
}

async function boot() {
  wire();
  await refreshStatus();
  if (!state.status) { toast("could not reach the API — log in again?", "err"); return; }
  state.activeId = state.status.active_session || null;
  if (state.activeId) {
    attachSessionStream();
    await loadHistory();
  }
  showTab("chat");
}

boot();
