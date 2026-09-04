# App channel (`/v1/*`)

The phone app talks to the assistant over the bearer-authenticated `/v1` routes
that Caddy reverse-proxies. It mirrors the **owner's own conversation** only:
group chats the assistant sits in never appear here.

| Route | What it does |
|---|---|
| `GET /v1/health` | liveness for the app itself |
| `GET /v1/messages` | pages the conversation (`after=` / `before=` / `limit=`) |
| `POST /v1/messages` | sends a turn (JSON `{"text"}` or multipart with `voice` / `photo` / files) |
| `GET /v1/events` | SSE stream of everything below |
| `POST /v1/reset` | starts a fresh agent session, like `/new` in Telegram |
| `POST /v1/push/register` | records the FCM token used when no SSE client is connected |

## SSE events on `GET /v1/events`

| `type` | Meaning |
|---|---|
| `message` | an unsolicited message (reminder, task result, `atlas-send`) |
| `typing` | a run is in flight; `since` is when it started |
| `progress` | one step of the run in flight, in `progress` |
| `token` | reserved for streamed output |
| `done` | terminal: the turn's reply, in `message` |
| `error` | terminal: the turn failed; the text is in `error` and in `message` |
| `interrupted` | terminal: the run in flight was killed by a newer message |

`typing` is also replayed on connect when a run is already going, so an app that
reconnects mid-turn shows the indicator immediately.

### `progress`

What the run is actually doing, so the indicator is not the only thing the app
can show during a turn that takes minutes.

```json
{"type":"progress","progress":{"kind":"tool_start","tool":"run_terminal_command",
 "detail":"ls -la","status":"in_progress","call_id":"call-6ee3…-0","seq":4,"elapsed_ms":20650}}
```

| Field | Meaning |
|---|---|
| `kind` | `tool_start`, `tool_end`, `thinking`, `end` |
| `tool` | the runner's tool name |
| `detail` | a redacted one-line summary, ≤80 runes; may be empty |
| `status` | `in_progress`, `completed`, `failed` |
| `call_id` | ties a `tool_end` to its `tool_start`, so a step is updated in place |
| `seq` | monotonic within the run |
| `elapsed_ms` | milliseconds since the run started |

`detail` is scrubbed before it leaves the process: credentials, long base64/hex
blobs and ANSI escapes are removed, and a tool's **output** is never forwarded at
all. Treat it as a label, not as data.

Progress is **advisory**: it is never persisted, it does not appear in
`GET /v1/messages`, and a client that misses one is not out of sync with
anything. `thinking` is emitted once per run, not once per reasoning token.

The last 64 events of the run in flight are **replayed on connect**, right after
the replayed `typing`. `kind: "end"` is terminal for the run's progress stream —
it arrives when the run finishes and equally when it is interrupted, so the app
can stop showing a step that will never be followed by another.

The same run is shown in Telegram as a single message that is edited in place and
then removed (or, for a background task, edited into the result); see
`docs/PROGRESS-STREAMING.md`.

### `interrupted`

A new message from either channel — Telegram or `POST /v1/messages` — **cancels
the conversational run in flight** instead of queueing behind it. It is the same
conversation from both sides, so a turn typed in the app interrupts one started
in Telegram and vice versa.

When that happens the assistant emits `interrupted` (no `message`: the killed
run's half-answer is discarded and never delivered on any channel), and then
immediately starts a replacement run that answers the interrupted turn and the
new one together. That replacement broadcasts its own `typing`.

So the app must **not** clear its indicator on `interrupted` alone — treat it as
"the pending turn is gone, expect a new `typing`", and drop any partial state
held for the turn that was cancelled. The event exists so the app has a terminal
signal for that turn rather than waiting on a `done` that will never come.

It replaced the old `queued` event (and its `queued` count field), which
announced that a message had been parked behind a running turn. Messages are no
longer parked.

Background work is never interrupted: `assistant_tasks` (including a turn
auto-promoted past the soft budget), scheduled jobs and `machine_ai_run` jobs
keep running and report through `message` as usual.
