# Takan Bots — daemon API contract

The **Bots** module turns Telegram assistant bots (first one: **Atlas**) into a Takan-managed
fleet, the same way the Machine module manages `takan-agent` installs.

- Takan owns the **fleet registry**, the **chat whitelist with approvals**, and a **hub → bot outbox**.
- The bot daemon owns the Telegram connection. **The Telegram bot token never reaches the hub.**
- A bot instance is also a `machine_ai_run` **owner**: when a job it launched finishes, the result
  is queued in that bot's outbox instead of being POSTed to an external webhook.

This document is the contract a daemon implements. Hub side is done; the daemon side is phase B.

## 1. Identity and auth

A bot is created in the panel (**Takan → Bots → Add bot**), which issues a **bot token** shown once.
The daemon stores that token in its own config and sends it on every call:

```
Authorization: Bearer <bot token>
```

- The token identifies the bot instance *and* the account. There is no other credential.
- `401` = unknown/absent token. `403` = the Bots module is disabled in the panel.
- **New token** in the panel rotates it: the old one stops working immediately.
- Every authenticated call counts as a heartbeat (the panel shows a bot as **online** for 3 minutes
  after its last call), so an idle long-poll also keeps the bot online.

Base URL is the hub's public URL, e.g. `https://takan.es`. All bodies are JSON.

## 2. Endpoints

### `POST /api/bots/register` — announce (idempotent)

Call at startup and after every reconnect.

```json
{ "name": "Atlas", "bot_username": "@atlas_bot", "machine": "vps2", "version": "0.3.1" }
```

All fields optional; empty fields leave the stored value untouched. `machine` is matched against
the account's machine names (Machine module) and links the bot to that machine when it matches.

`200`:

```json
{
  "bot": { "id": "…", "name": "Atlas", "bot_username": "atlas_bot", "machine": "vps2",
           "version": "0.3.1", "last_seen": "2026-09-04T10:00:00Z" },
  "chats": [ /* full whitelist, see §3 */ ],
  "cursor": "2026-09-04T09:58:00Z",
  "note": "optional: the token belongs to a differently named bot"
}
```

**Takan's name wins.** If the daemon's configured `name` differs, the response carries `note` and
the daemon should adopt `bot.name` (renaming is a panel action).

### `POST /api/bots/heartbeat` — liveness

Empty body. Call every ~60 s when not long-polling.

```json
{ "ok": true, "bot": "Atlas", "cursor": "2026-09-04T09:58:00Z", "pending": 1,
  "now": "2026-09-04T10:00:00Z" }
```

### `GET /api/bots/chats` — the whitelist

Query params (all optional):

| Param | Meaning |
|---|---|
| `status` | `pending` \| `approved` \| `denied` — filter |
| `updated_since` | RFC3339; only chats changed **after** that instant |
| `wait` | seconds (max 60) — long-poll: block until something changes, then return |

```json
{
  "bot": "Atlas",
  "chats": [
    { "chat_id": "282611642", "type": "private", "title": "Jairo", "username": "kidandcat",
      "status": "approved", "first_message": "hey", "decided_by": "panel",
      "created_at": "2026-09-04T09:50:00Z", "updated_at": "2026-09-04T09:58:00Z" }
  ],
  "cursor": "2026-09-04T09:58:00Z"
}
```

**Recommended loop:** keep an approved-chat set in memory; poll
`GET /api/bots/chats?updated_since=<cursor>&wait=55` in a loop, merge the returned rows, store the
new `cursor`. A decision made in the panel or by an MCP tool wakes the long poll immediately, so
approvals land in seconds. A missed wake-up self-heals on the next poll (the cursor is durable).

`type` is `private` or `group` — Telegram `supergroup`/`channel` are normalised to `group`.
**Approving a group authorises the whole group** (every member), by design.

### `POST /api/bots/chats/pending` — report an unknown chat

Call the first time an unknown chat writes to the bot. **Do not answer it yet.**

```json
{ "chat_id": "-1002233445566", "type": "supergroup", "title": "Casa",
  "username": "", "first_name": "", "last_name": "", "first_message": "hola" }
```

`title` falls back to `first_name + last_name`; `first_message` also accepts the alias `snippet`.

`201` when the chat was newly recorded, `200` when it was already known:

```json
{ "chat": { …chat object… }, "created": true }
```

Idempotent per `(bot, chat_id)`: repeating it refreshes display info but **never** resets an
`approved`/`denied` decision back to pending. The operator is notified on Telegram **once**, on
first report (bot name + who + snippet + a link to the panel).

### `GET /api/bots/deliveries` — the outbox (hub → bot)

Query params: `limit` (default 20, max 50), `wait` (seconds, max 60 — long-poll).

```json
{
  "bot": "Atlas",
  "deliveries": [
    { "id": "0d6f…", "type": "ai_job_result", "attempts": 1,
      "created_at": "2026-09-04T10:00:00Z",
      "payload": {
        "job_id": "job-42", "machine": "vps2", "runner": "grok", "status": "done",
        "exit_code": 0, "parent_job_id": "", "finished_at": "2026-09-04T09:59:58Z",
        "output_tail": "…last 4000 bytes of the transcript…", "truncated": true,
        "requested_chat_id": "282611642"
      } }
  ]
}
```

- `payload` shape depends on `type`. Today the only type is **`ai_job_result`**.
- `requested_chat_id` is the Telegram chat that asked for the job (passed as `chat_id` to
  `machine_ai_run`). When empty, deliver to the daemon's default operator chat.
- `output_tail` is at most 4000 bytes, prefixed with `…` when cut; the full transcript is still
  available through `machine_ai_log`.
- **Only deliver to approved chats.** A delivery does not bypass the whitelist.

### `POST /api/bots/deliveries/ack` — confirm

```json
{ "ids": ["0d6f…", "91ab…"] }
```

(`{"id": "0d6f…"}` is accepted for a single one.)

```json
{ "acked": 2, "pending": 0 }
```

**At-least-once.** A fetched delivery is invisible for 30 s, then handed out again until acked — so
a daemon that crashes after fetching but before forwarding gets it back. Acking an unknown or
already-acked id is a harmless no-op. Acked rows are purged after 7 days.

## 3. Daemon loop (reference shape)

```
POST /api/bots/register              → adopt bot.name, seed the whitelist, keep cursor
loop:
  GET /api/bots/chats?updated_since=<cursor>&wait=55   (goroutine A) → merge, update cursor
  GET /api/bots/deliveries?wait=55                     (goroutine B) → forward, then ack
  POST /api/bots/heartbeat every 60s if neither poll is in flight
on incoming Telegram message:
  chat approved?  → handle it
  chat unknown?   → POST /api/bots/chats/pending, stay silent
  chat denied?    → stay silent
```

Two long polls means two in-flight requests per bot; both are cheap (a blocked goroutine on the hub).

## 4. Machine AI jobs owned by a bot

`machine_ai_run` (and `machine_ai_reply`) take:

- `owner` — **a bot name** from `bots_list`. Required.
- `chat_id` — optional Telegram chat to answer in; it is stored with the job and travels back in
  `requested_chat_id`.

The tool result includes a `delivery` line stating where the result will go
(`"queued to bot Atlas for chat 282611642 when the job finishes"`), or why it will go nowhere.

When the job reaches `done` / `failed` / `cancelled`, Takan queues one `ai_job_result` delivery for
the owning bot (deduped per job id while it is still queued). MCP clients keep receiving the
`notifications/takan/machine_ai_job` SSE notification exactly as before — the outbox is additive
for bots, not a replacement for it.

Jobs launched with an `owner` that is not a registered bot still run; only the delivery is skipped.

### Legacy owners

`machine_ai_run owner` used to be a fixed enum (`Minerva, Menta, TPVLINE, Gestor, Hardware, Games`)
of "Grok Bots". Those names are now seeded as **legacy bot rows** on accounts that already existed,
so old callers keep resolving. A legacy row has no token and no daemon; the panel can **Issue
token** on it to turn it into a real bot instance.

## 5. MCP tools

Available while the module is enabled:

| Tool | Purpose |
|---|---|
| `bots_list` | fleet: name, machine, online, kind, pending chats, queued deliveries |
| `bots_chats` | chats of one bot (or all), filterable by status |
| `bots_approve` | approve a chat (a group approves everyone in it) |
| `bots_deny` | deny a chat |

`takan_status` reports module readiness (`x/y online · n chat(s) pending approval`).

## 6. Security notes

- The bot token is the only credential; treat it like the agent token. Rotate from the panel.
- Everything is scoped to the owning account: a bot can only ever see its own chats and outbox.
- The hub never stores the Telegram bot token, and never talks to Telegram on the bot's behalf
  (the pending-approval alert goes through the operator's own Telegram module).
- Chats default to **denied by omission**: a daemon must treat anything not `approved` as silent.
