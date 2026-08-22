# Takan MCP events vs Grok Bot wakeup

Follow-up to [TAKAN_OSS_SELFHOST.md](TAKAN_OSS_SELFHOST.md). Question: does Takan push events to an MCP client (Grok Bot), and can that client receive them and **start a new model turn** (so Minerva notices a finished `grok` job without Hairok asking “how is it going”)?

**Date:** 2026-08-21  
**Code:** `feat/oss-selfhost` (same tree as `master` @ `0f2df2b` for this path). Production hub `/opt/takan` was **not** changed.  
**Clients checked:** Takan MCP server, `takan-agent` job wire, live `journalctl -u takan` on this VPS, Grok Build MCP client (`xai-org/grok-build` `GrokClientHandler`), xAI Remote MCP + Automations docs. Local `grokbot` is **not** an MCP client of Takan.

**Verdict:** Takan **does** emit two JSON-RPC notifications on open Streamable-HTTP **GET `/mcp` SSE** streams. Grok Bot **can receive the bytes** (it keeps GET SSE open against `takan.es`). It **does not wake the agent** on `notifications/takan/machine_ai_job`. Grok Build only handles `tools/list_changed` and `resources/list_changed`, and those become a UI status ping (`x.ai/mcp/server_status`), not a new prompt. After the chat turn that called `machine_ai_run` ends, Minerva is deaf until a human (or an out-of-band webhook/cron) speaks.

---

## 1. Events that ARE implemented in Takan

Push path is always `SessionHub.Notify` → SSE `data:` frames on **GET `/mcp`**. POST `/mcp` is request/response JSON (`writeJSON`); it is **not** a server-push channel. There is no webhook outbound, no MCP `resources/`, no MCP sampling, no `notifications/message` logging.

### 1.1 `notifications/takan/machine_ai_job` (custom)

| | |
|--|--|
| **Method** | `notifications/takan/machine_ai_job` |
| **When** | Agent WebSocket message `ai_done` (job reached **terminal** status: `done` / `failed` / `cancelled`). Wired in `cmd/takan/main.go` via `hub.OnJobEvent`. |
| **Who emits** | `takan-agent` `jobManager.emit` → WS `{type:"ai_done", ...}` → hub `HandleWS` case `"ai_done"` → `OnJobEvent`. |
| **Params** | `machine`, `job_id`, `status`, `exit_code`, `runner`, `parent_job_id`, `finished_at`. **No log tail, no prompt.** |
| **Not fired** | Job *start*; mid-run progress; `machine_ai_status` polls; email/telegram/mercadona/display/sip/vault. |
| **Lossy** | Best-effort. Missed if the agent is disconnected when the process exits (`JobEventHandler` comment). No queue, no replay. Slow SSE consumers are dropped (`broadcast` default branch). Hub log `mcp: notify user=… method=… streams=N` only when **N > 0**. |

Tests: `internal/mcp/session_test.go`, `internal/agenthub/hub_test.go` (`expected OnJobEvent from ai_done`).

### 1.2 `notifications/tools/list_changed` (MCP spec)

| | |
|--|--|
| **Method** | `notifications/tools/list_changed` |
| **When** | Panel / mobile API changes the user’s tool set: module toggle, vault, AI runners, Mercadona link, display, etc. (`web.OnToolsChanged`, `api.OnToolsChanged` → `mcp.Server.NotifyToolsChanged`). |
| **Params** | none (`params` omitted). |
| **Capability** | `initialize` advertises `capabilities.tools.listChanged = true` only. No `resources`, no `logging`. |
| **Intent** | Client should call `tools/list` again. Takan **does not** drop the session / 401 (Grok mobile re-auth is painful — comment in `internal/mcp/server.go`). |

### 1.3 Client → server (accepted, no-op)

POST methods `notifications/initialized` and `notifications/cancelled` return empty JSON-RPC results. They do not fan out.

### 1.4 Transport details (GET SSE)

- Streamable HTTP protocol version `2025-03-26`.
- GET without `MCP-Session-Id` creates a listen-only session.
- Keepalive comments every 25s; **hard cap 15 minutes** (`sseMaxLifetime`) then the server closes; clients must reconnect (`3b66d38`).
- Channel buffer 8.

Live `takan.es` (this VPS, 14 days of journal, user `kidandcat@gmail.com` / `e824c2c7-…`): **~1221** `mcp: SSE max lifetime reached` lines (≈ 1–2 standing GET streams). **Zero** `mcp: notify` / `list_changed_streams` lines in the same window — either no terminal `ai_done` landed while a stream was open, or `Notify` returned 0 (silent). The binary **does** contain `notifications/takan/machine_ai_job` (deployed 2026-08-21 19:13). Recent `machine_ai_run` log lines are errors (30s timeout, bad `cwd`), not completions.

---

## 2. What is polling-only (no MCP push)

| Surface | How the client learns | Push to MCP? |
|---------|------------------------|--------------|
| **AI job still running / after a missed `ai_done`** | `machine_ai_status` (snapshot + tail), `machine_ai_log` (full transcript), `machine_ai_watch` (block ≤300s). | Watch is **hub-side**: waiter on `ai_done` **plus** status poll every 1.5s. That unblocks the **in-flight `tools/call`**, not a new Grok turn. |
| **Email inbound** | `email_list` / `email_get` against Resend. No Resend webhook into the hub. | No |
| **Telegram inbound** | `getUpdates` is panel chat discovery (`DiscoverChats`), not a bot webhook and not MCP. Tools are `telegram_chats` / `telegram_send` (outbound). | No |
| **Mercadona** | Request/response tools. | No |
| **Display** | Hub → agent WS `display` (kiosk HTML). Agent kiosk page has `EventSource('/events')` **on the machine**, not on MCP. | No |
| **SIP / Grok Voice** | `OnEvent` only `log.Printf` (`session.created`, `response.done`, …). | No |
| **Vault grants** | `secrets_request` then poll `secrets_status`. | No (`list_changed` may fire if vault settings change). |
| **People / health** | CRUD tools. | No |

There is **no** Takan HTTP webhook to call Grok, grokbot, Telegram, or ntfy when something happens.

---

## 3. Does Grok Bot receive them and wake?

Three different “Grok”s:

### 3.1 Grok Bot on grok.com / mobile (Hairok’s Takan MCP)

This is the client holding GET `/mcp` on `takan.es` (SSE lifetime logs). So **yes, the HTTP client is listening**, and a `machine_ai_job` frame **can be delivered** if a stream is open at `ai_done`.

There is **no** Takan-side or public xAI-doc evidence that an unknown JSON-RPC method starts a new chat turn. MCP clients are supposed to ignore methods they do not implement. Takan’s own tool text already hedges: “Clients with an MCP SSE stream **may** also receive …”.

xAI **Remote MCP Tools** (API / Responses) attach MCP to a **single** `chat.stream()` / `responses.create`. When that request finishes, the managed connection is gone — idle Minerva is not sitting in that path.

xAI’s documented way to **start** Grok from outside is **Automations → Webhook trigger** (Standard Webhooks HMAC, `202 Accepted`). Takan does not call that.

### 3.2 Grok Build CLI (`grok` — source of truth for the MCP client)

`GrokClientHandler` in `xai-org/grok-build` (`crates/codegen/xai-grok-mcp/src/servers.rs`):

> when the server pushes a notification we care about — currently `notifications/tools/list_changed` and `notifications/resources/list_changed`.

Implemented methods: `on_tool_list_changed`, `on_resource_list_changed` only. Those emit `McpClientEvent::ToolsChanged` / `ResourcesChanged`, coalesced into ACP **`x.ai/mcp/server_status`** (UI: server still `ready`, reason `config_changed`). That refreshes tool chrome. **It is not `session/prompt`.** Custom `notifications/takan/*` never hits a handler.

This VPS `grok mcp list` → *No MCP servers configured.* Minerva’s Takan link is the hosted Bot, not this CLI.

### 3.3 `grokbot` on this host (`grokbot.jairo.cloud`)

Self-hosted crew that execs `grok -p`. SSE in that repo is **browser ← grokbot** (thread events), not Takan → grokbot. No `takan` / `machine_ai` references. Atlas routines table is empty. **Not in the wakeup path.**

**Bottom line:** Grok Bot **receives** SSE (including custom notifications) and **ignores** `notifications/takan/machine_ai_job` for agent wakeup. `tools/list_changed` may refresh tools; it will not make Minerva talk.

---

## 4. What is missing so Minerva notices a finished grok alone

Desired loop: Minerva calls `machine_ai_run` → turn ends → minutes later the machine grok exits → Minerva starts a turn, reads `machine_ai_log`, replies.

**Works today (same turn only):**

1. Call `machine_ai_watch` after `machine_ai_run` (default 90s, max 300s, repeat on `timed_out`). The watch RPC stays open; Grok’s turn is still alive. This is polling + `ai_done` inside the hub, **not** an SSE wakeup.
2. If Minerva forgets to watch and the user is gone, **nothing** happens.

**Not implemented (needed for idle wakeup):**

| Gap | Why it matters |
|-----|----------------|
| **No durable inbox** | SSE is fire-and-forget. 15 min cap, reconnect races, agent offline → event vanished. Need a per-user event table + `events_list` / MCP resource. |
| **No client mapping to a new turn** | Even a delivered `machine_ai_job` is ignored by Grok Build / Bot. Fix is on the **client** (treat as `session/prompt`) or an **out-of-band** trigger Grok already honors. |
| **No Takan → Grok Automations webhook** | Documented Grok wake API. `OnJobEvent` could POST Standard Webhooks to a saved Minerva automation. Config + secret in the panel; do not hardcode grok.com. |
| **No Takan → grokbot / Telegram nudge** | Household alternative: `telegram_send` or grokbot HTTP when `ai_done`. Still product work. |
| **No events for mail / Mercadona / Telegram inbound** | Those modules are pull tools. Inbound mail will not poke Minerva either. |
| **Watch timeout vs long groks** | 300s max per call. A long job needs chained watches **in the same turn**, or the webhook path. |

Recommended order if we implement later (do **not** ship to `/opt/takan` without an explicit deploy):

1. **P0 product:** document for Minerva “always `machine_ai_watch` until terminal” (already in tool descriptions; the model still drops it).
2. **P0 wakeup:** `OnJobEvent` → optional Grok Automations webhook (BYO URL + `whsec_`) and/or Telegram. This is the only path that wakes hosted Grok Bot without xAI changing MCP.
3. **P1:** persist last N job events per user; add `machine_ai_events` or a resource so a heartbeat/cron can catch missed SSE.
4. **P2:** do **not** expect `notifications/takan/*` to wake Grok until Grok documents it. Optional: also emit `notifications/tools/list_changed` on job end — Grok would refresh tools, **still** not start a turn.

No production deploy from this note.
