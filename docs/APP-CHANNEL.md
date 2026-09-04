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
| `token` | reserved for streamed output |
| `done` | terminal: the turn's reply, in `message` |
| `error` | terminal: the turn failed; the text is in `error` and in `message` |
| `interrupted` | terminal: the run in flight was killed by a newer message |

`typing` is also replayed on connect when a run is already going, so an app that
reconnects mid-turn shows the indicator immediately.

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
