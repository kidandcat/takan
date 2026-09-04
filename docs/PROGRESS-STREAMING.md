# Live progress streaming

During a conversational run or a background task (often minutes) the operator
used to see only a typing indicator: no idea which tool was running, on what, or
how far along. Now every run reports its steps into **exactly one Telegram
message, edited in place**, and onto the app's SSE channel.

The rule this is built around: **one message per run, never a stream of them.**
A conversational run's message is deleted the moment its real answer is posted;
a background task's message is *edited into* the result, because that message is
the task's only trace in the chat.

## Source: the runner's NDJSON stdout

`grok` (1.0.13) exposes a headless event stream:

```
--output-format <OUTPUT_FORMAT>   plain | json | streaming-json | streaming-messages-json
```

`streaming-json` writes one ACP session update per line, **in real time** — not
flushed at the end. A capture of a two-tool run (`internal/assistant/testdata/grok-streaming.ndjson`,
279 lines) contains 216 `text`, 44 `thought`, 2 `tool_call`, 8 `tool_call_update`,
5 `available_commands`, 3 `usage`, 1 `end`.

```json
{"type":"tool_call","toolCallId":"call-6ee3…-0","toolName":"run_terminal_command",
 "status":"pending","rawInput":{"command":"ls -la","description":"List all files…"}}
{"type":"tool_call_update","toolCallId":"call-6ee3…-0","status":"completed",
 "content":[{"type":"content","content":{"type":"text","text":"total 48\n…"}}],
 "rawOutput":{"type":"Bash","exit_code":0,"command":"ls -la","current_dir":"…"}}
{"type":"text","data":"`notes.txt` has **3** lines."}
{"type":"usage","usage":{…},"signature":"<opaque base64>"}
{"type":"end","stopReason":"end_turn","sessionId":"…","total_cost_usd":0.0144}
```

**The delta field is `data`, not `text`.** The answer is no longer stdout: it is
the concatenation of every `text` delta. `testdata/grok-streaming.answer.txt` is
the reply that concatenation produces, and `TestStreamRebuildsTheAnswerFromTheDeltas`
pins it.

Stdout was chosen over tailing `~/.grok/sessions/<cwd>/<uuid>/events.jsonl`:
lower latency, no path derivation from a percent-encoded cwd, no poll loop, no
cleanup. (`events.jsonl` also revealed, incidentally, that MCP server startup
alone accounts for ~32s of the perceived silence at the start of a run.)

## Failure mode, chosen deliberately

The reply now depends on parsing a format the runner does not promise to keep
stable, so:

- The format lives in the **arguments** (`--output-format`), which is the single
  source of truth for both the runner and the parser (`outputFormatOf`).
- A run whose stdout contains **no line matching the known schema** delivers that
  stdout verbatim. A runner upgrade that changes the format therefore costs the
  progress display, never the answer. (`TestStreamFallsBackWhenTheSchemaIsUnknown`)
- Stray lines *before* a valid stream (a warning, a deprecation notice) are
  dropped, not delivered. (`TestStreamPrefersTheSchemaOverStrayLines`)
- A line too big to be worth reading (a tool dumping a whole file into its
  update) is discarded, but the pipe **keeps draining** — stopping the reader
  would block the child on a full pipe. (`TestStreamDrainsAMonsterLine`)
- The panel has a **Show live run progress** checkbox. Off puts the runner back
  on `--output-format plain` and stdout back to verbatim.

An instance configured before this existed has `--output-format plain` saved in
`user_modules`. `applyDefaults` **rewrites** the flag rather than only defaulting
it, or the feature would be silently off on the machine that has been running
longest. (`TestStoredPlainArgsAreUpgraded`)

## Redaction (mandatory)

A tool call's arguments are model-authored and routinely carry the operator's own
secrets. Everything published goes through `scrubDetail`:

- tool **output** is never forwarded at all — not `tool_call_update.content`, not
  `rawOutput`. That is where file contents and credentials actually live;
- `rawInput` is never forwarded verbatim: one field is picked from a fixed list
  (`command`, `file_path`, `path`, `pattern`, `query`, `url`, `target_file`,
  `description`) and scrubbed;
- `(?i)(token|secret|key|password|passwd|bearer|authorization|api[_-]?key)\s*[=:]\s*(bearer\s+)?\S+`
  → `label=<redacted>`, plus a bare `bearer <token>`;
- base64/hex runs of ≥24 characters → `<redacted>` (`/` is outside the class so
  ordinary paths survive);
- ANSI escapes stripped, whitespace collapsed, capped at 80 runes;
- `usage.signature` is dropped by never being declared in the parsed struct.

## Telegram UX

The message is created on the **first tool call** — a turn answered straight away
never produces one — and shows the last 4 steps plus elapsed time:

```
⏳ Tarea a1b2c3 — count the lines        ← header, background tasks only
⚙️ bash: ls -la
✅ read: notes.txt
⏱ 1m 23s
```

- Sent as plain text (a redacted command line is not Markdown).
- Edited at most every **2.5s**, coalescing whatever arrived in between; an edit
  whose body is unchanged is skipped (Telegram calls that `message is not
  modified`, which the client treats as success).
- On a **429** the display backs off by `retry_after` and doubles its interval;
  after a **second** 429 it stops editing for the rest of the run. This chat also
  carries the real conversation, and the answer must not be the request that gets
  refused.

| Run ends by | What happens to the message |
|---|---|
| conversational answer | deleted, immediately before `deliverAssistant` |
| interrupt, `/cancel`, `/new`, error, restart | deleted |
| promoted past the soft budget | the "moved to background" notice becomes its **header**; it keeps updating and is later edited into the result |
| background task completes | edited into the result |
| background task result > 4096 runes | edited into a one-line `✅ Tarea … · resultado abajo ↓`, and the result follows through the chunked send — the one acceptable extra message |

`atlas-task run` shows its message straight away (header only), so a task
acknowledges itself instead of going silent until it finishes. It is still one
message: the same one is edited throughout and into the result.

### Outbound choke point

Editing and deleting go through `Emit` like everything else (`Outbound.Kind` =
`edit` / `delete`, `Outbound.MessageID`), and `TestOnlyEmitTalksToTelegram`
enforces it at the source level for `EditMessageText` / `DeleteMessage` too.

Progress frames set `Outbound.SkipHistory`: they live in Telegram alone, because
recording each frame would fill the phone's conversation with lines that no
longer exist in the chat. A background task's **final** edit does not set it —
that text is the task's answer and is recorded exactly once, through the same
path every other assistant message takes.

## App channel

- New SSE type **`progress`** carrying `AppEvent.Progress` (`kind`, `tool`,
  `detail`, `status`, `call_id`, `seq`, `elapsed_ms`).
- A per-run ring buffer of 64 events is replayed at connect time, right after the
  replayed `typing`, so a client that reconnects mid-run is level with one that
  was listening from the start.
- `eventBuffer` went from 16 to 64: a run now emits an event per tool call on top
  of the turn's own events.
- A terminal `progress` event with `kind: "end"` closes the stream, including
  when the run was interrupted.

See `docs/APP-CHANNEL.md`.

## Where it lives

| File | What |
|---|---|
| `internal/assistant/progress.go` | event shape, redaction, NDJSON line parser, ring buffer, Telegram renderer |
| `internal/assistant/progressmsg.go` | `progressTracker`: the one message, throttle, coalescing, 429 policy, delete / finish / announce |
| `internal/assistant/agent.go` | `StdoutPipe` + line reader, answer reconstruction, fallback, `RunSpec.OnProgress`, `RunHandle.Progress()` |
| `internal/assistant/options.go` | `AgentOpts.Progress`, `withOutputFormat`, `outputFormatOf` |
| `internal/tg/client.go` | `EditMessageText`, `DeleteMessage`, `retry_after` on `APIError` |
| `internal/assistant/bot.go` | tracker per conversational run, delete on finish/interrupt, promotion |
| `internal/assistant/tasks.go` | per-task tracker, edit-into-result |
| `internal/assistant/sse.go`, `appapi.go` | `progress` event and replay on connect |
