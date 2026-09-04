# Takan Bots — daemon API contract

The **Bots** module turns Telegram assistant bots (first one: **Atlas**) into a Takan-managed
fleet, the same way the Machine module manages `takan-agent` installs.

- Takan owns the **fleet registry**, the **chat whitelist with approvals**, and a **hub → bot outbox**.
- The bot daemon owns the Telegram connection. The Telegram credential is no longer the daemon's
  private secret: it belongs to a **channel** (§4), is sealed at rest in the hub, and reaches the
  daemon through its channel attachment.
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

`bot.name` is **guaranteed present and non-empty** on every `200`. The stored name always wins over
whatever the daemon sent, and the hub never returns it blank, so a daemon can adopt it
unconditionally (no fallback to its own configured `name` is needed, and no empty-string check).

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
| `updated_since` | RFC3339; only chats with `updated_at` **strictly after** that instant (exclusive) |
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

**The boundary is exclusive** (`updated_at > updated_since`), never inclusive. `cursor` is the
newest `updated_at` in the returned set, so re-sending the last received `cursor` returns only
genuinely newer rows: the row that produced the cursor is not handed back, and there is no eternal
replay loop of the same chat. The daemon's merge should still be idempotent anyway (re-applying a
row it already has must be a no-op), because a row can legitimately be returned again after any
later edit, and because the first poll after a restart replays the whole whitelist.

`type` is `private` or `group` — Telegram `supergroup`/`channel` are normalised to `group`.
**Approving a group authorises the whole group** (every member), by design.

### `POST /api/bots/chats/pending` — report an unknown chat

Call the first time an unknown chat writes to the bot. **Do not answer it yet.**

```json
{ "chat_id": "-1002233445566", "type": "supergroup", "title": "Casa",
  "username": "", "first_name": "", "last_name": "", "first_message": "hola" }
```

`title` falls back to `first_name + last_name`. `first_message` is the **canonical and preferred**
field name; `snippet` is still accepted as an alias for compatibility with older daemons, and
`first_message` wins when both are sent.

`type` accepts **both** the raw Telegram values (`private`, `group`, `supergroup`, `channel`) and
the already-normalised ones (`private`, `group`) — a daemon may forward Telegram's `chat.type`
verbatim without pre-mapping it. The hub normalises server-side to `private|group`, and that
normalised value is what the whitelist stores and what every response (here, `GET /api/bots/chats`,
the panel and the MCP tools) reports back.

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
- `payload.exit_code` is **only meaningful when `status == "failed"`**. On `done` and `cancelled`
  the field is still present but its value is unspecified (typically `0`) and carries no
  information, so daemons should not surface it. Rendering it only when
  `status == "failed" && exit_code != 0` is the correct behaviour.

**Unknown `type` values: ack and log.** New delivery types may appear without a daemon change. A
daemon that does not recognise a `type` must **log it and ack it anyway**, not hold it unacked. The
hub does *not* want unknown types parked in the outbox: an unacked row is re-delivered every 30 s
and the outbox would grow forever. Acking makes every future type **opt-in** — a daemon starts
handling one only once it has been taught to, and until then the row is discarded cleanly.

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

## 4. Telegram channels

A **channel** is the first-class Telegram entity in Takan. The Bots module no longer holds Telegram
tokens of its own: a bot instance receives both its credential and its primary chat by being
*attached* to a channel. The data model lives in `internal/store/telegram_channels.go`.

| Entity | Fields | Notes |
|---|---|---|
| **Channel** | `name`, BotFather token, `bot_username`, `bot_display_name`, `is_default` | The token is pasted once, validated with `getMe`, then sealed with the vault's `cryptox.Box`. It is never stored or logged in clear. `bot_username` and `bot_display_name` come from that same `getMe`. |
| **Channel chat** | `chat_id`, `type` (`private` \| `group`), `label` | A channel has one or more chats (for example a DM plus a family group). |
| **Attachment** | `channel`, `consumer`, `consumer_id`, `direction`, `chat_id` | Many-to-many binding between channels and consumers. |

**Directions:**

| `direction` | Meaning |
|---|---|
| `receive` | The consumer processes inbound messages arriving at that bot (typically a bot daemon instance running `getUpdates`). |
| `send` | The consumer emits messages to the channel: the internal operator notifier (`telegram_send`), email-module notifications, AI-job result delivery. |

`consumer` is one of `bot` (with `consumer_id` = the bot id), `notifier` or `email`. The
attachment's `chat_id` is that consumer's primary chat within the channel; when empty it means "the
channel's first chat".

**Constraint (important):** a Telegram bot token supports a single `getUpdates` consumer, so a
channel may have **at most one `receive` attachment**. This is enforced by a partial unique index,
not just by convention. `send` attachments are unlimited: one channel can serve many senders, and
one consumer can use many channels.

**Migration and seeding.** The credential and operator chat the Telegram module used before
channels existed are seeded as the channel named `default`, together with its chats and a
`notifier` send-attachment, so existing notification behaviour is unchanged.

**What this means for a daemon.** A bot instance gets both its Telegram credential and its
owner/primary chat from its channel attachment. For a group bot the channel points at the group
chat id, so the daemon is born already pointed at its group and does not need an approval round for
it. The pending/approval whitelist (§2, §3) still governs any **other** chat that messages the bot.

Forward note: the standalone resend to Telegram service currently running on vps2 stays as it is
for now, but this attachment model is what it will bind to when it is absorbed into the hub.

## 5. Zero-touch provisioning and binary hosting

Takan can install a bot daemon on a machine without anyone copying files by hand: the hub itself
serves the daemon binaries, and provisioning pulls the right one over the same authenticated API.
The contract below is what a daemon repo's deploy has to satisfy (phase B on the atlas side is
extending its deploy to upload the built binaries).

| Item | Value |
|---|---|
| Binary directory | `/opt/takan/bot-binaries/` on the hub host (default) |
| Override | env `TAKAN_BOT_BIN_DIR` |
| Filename layout | `<name>-<os>-<arch>`, for example `atlas-linux-amd64`, `atlas-linux-arm64` |
| File mode | must be executable |
| Endpoint | `GET /api/bots/binary?os=linux&arch=amd64` |
| Response | `application/octet-stream` with the binary body |
| Auth | `Authorization: Bearer <token>`, accepting **either** a machine agent token **or** a short-lived provision ticket |
| Not found | `404` when no binary matches the requested `os`/`arch` |

Concretely, the **atlas** deploy should upload its freshly built binary to
`/opt/takan/bot-binaries/atlas-linux-amd64` on the hub host (vps2), preserving the executable bit
(`scp` then `chmod +x`, or `install -m 0755`). Publishing extra `os`/`arch` combinations is just a
matter of dropping more files with the same naming pattern next to it; the hub resolves the request
purely by filename, so no registration step is needed. v1 targets **Linux with systemd only**:
provisioning refuses cleanly (with an explanatory error, no partial install) on machines without
`systemctl`.

### What provisioning installs

The hub generates a fixed script (the panel never supplies shell) and runs it through the target
machine's takan-agent over the existing `bash` transport, so machines already in the field need no
agent update. Secrets never reach the command line: the script fetches them from the hub with a
short-lived, run-scoped ticket, so the Telegram token never appears in the target's process list.

| Path on the machine | Contents |
|---|---|
| `/usr/local/bin/<instance>` | the daemon binary, mode `0755` |
| `/etc/<instance>/<instance>.env` | the environment file, mode `0600` in a `0700` directory |
| `/etc/systemd/system/<instance>.service` | the unit (`Restart=on-failure`, `MemoryMax=512M`, `MemorySwapMax=0`, `NoNewPrivileges`, `PrivateTmp`) |

`<instance>` is derived from the bot name (lowercased, non-alphanumerics collapsed to `-`).
The env file contains exactly the four variables the daemon reads, and nothing else:

```
TELEGRAM_BOT_TOKEN="…"   # from the attached channel's sealed credential
ALLOWED_CHAT_ID="…"      # the attachment's primary chat within that channel
TAKAN_HUB_URL="…"        # the hub public URL; presence flips the daemon into hub mode
TAKAN_BOT_TOKEN="…"      # the bot's hub token, minted server-side at provision time
```

Provisioning is idempotent: re-running it refreshes the binary and the env file and restarts the
unit. The run is reported in the panel as `queued` -> `running` -> `ok` / `failed` (with the last
error), and the outcome is pushed to the operator over Telegram. It exits non-zero, leaving the
error visible, when the machine has no `systemd`, when the agent user has neither root nor
passwordless `sudo`, or when the unit is installed but does not stay active.

## 6. Machine AI jobs owned by a bot

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

## 7. MCP tools

Available while the module is enabled:

| Tool | Purpose |
|---|---|
| `bots_list` | fleet: name, machine, online, kind, pending chats, queued deliveries |
| `bots_chats` | chats of one bot (or all), filterable by status |
| `bots_approve` | approve a chat (a group approves everyone in it) |
| `bots_deny` | deny a chat |

`takan_status` reports module readiness (`x/y online · n chat(s) pending approval`).

## 8. Security notes

- The bot token is the only credential; treat it like the agent token. Rotate from the panel.
- Everything is scoped to the owning account: a bot can only ever see its own chats and outbox.
- Telegram bot tokens live in **channels** (§4), sealed with the vault's `cryptox.Box`, never
  stored or logged in clear and never echoed back by the API. The hub does not talk to Telegram on
  a bot's behalf (the pending-approval alert goes through the operator's own notifier attachment).
- Chats default to **denied by omission**: a daemon must treat anything not `approved` as silent.

## 9. Operating gotchas

### Never let a root-run job write a service user's home

`takan-agent` runs as **root** on the VPS boxes, so anything `machine_ai_run`
launches runs as root too. On vps2 `/usr/local/bin/grok` is a wrapper that pins
`HOME=/home/debian`, so a root-launched grok job rewrote
`/home/debian/.grok/auth.json` as `root:root`. Atlas runs as `debian`, and its
next turn failed with "Not signed in".

Rules that follow:

- The vps2 wrapper now drops back to `debian` (`runuser -u debian`) whenever it is
  invoked as root, so agent jobs, cron and a stray `sudo grok` are all safe. Keep
  that guard if the wrapper is ever regenerated.
- **Never invoke `grok` as root on vps2.**
- Anything that reads a service user's home (a future runtime-bundle import) must
  read it **read-only**: plain `sudo cat` / `cp` of the files, never executing
  `grok`, never triggering a token refresh, and never changing ownership or
  permissions of anything under `/home/debian`.
- After such an import, assert `~/.grok/auth.json` is still owned by the service
  user and mode 0600. If it is not, the import re-owned it and the daemon is
  about to fail with "Not signed in".

### A daemon's reported machine name is only a hint

Daemons report the host's raw hostname (`vps-068ca265`), not the machine name the
operator configured (`vps2`). The hub adopts the reported value only when it
resolves to a machine of that account, or when the bot has no machine recorded
yet; otherwise the configured name wins.
