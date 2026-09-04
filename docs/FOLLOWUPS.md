# Follow-ups

Known gaps left open after the assistant merge, with the reasoning for leaving
them. Each one was looked at and deliberately deferred; none is a placeholder
for "we forgot".

## M1 — the prompt travels in argv

`Agent.Start` passes the user's message as a command-line argument
(`{prompt}` substituted into `Args`). On Linux `/proc/<pid>/cmdline` is readable
by any process of the same user, so anything else running as `debian` can read
the operator's messages while a run is in flight. It is also visible in `ps`.

Not fixed here because the fix is not local: the runner's CLI decides how it
accepts a prompt, and `grok --single "<prompt>"` is the documented interface.
Doing it properly means either stdin (needs a runner that reads it, and a
different `Args` template per runner) or a temporary prompt file passed by path
(leaks to disk instead, and needs cleanup on kill). Both change the panel's
runner configuration shape.

Mitigations already in place: the hub runs as a single-user box, and the child
environment is an allowlist, so the argv exposure is the only channel left.

## L2 — no rate limit on the assistant's own inbound

`/v1/messages` is bearer-authenticated and Telegram is gated on one user id, so
neither is open to strangers. But a compromised app token, or the operator's own
looping script, can queue up to `queueSize` messages per chat and start one
agent run each. `MaxConcurrentTasks` bounds background tasks; conversational
runs are bounded only by the per-chat serialisation.

## L3 — SSE fan-out drops slow clients silently

`EventBus.Broadcast` skips a subscriber whose buffer is full rather than
blocking the agent run, which is the right trade. The client is expected to
notice the gap and re-page `/v1/messages`, but nothing tells it that a drop
happened — there is no sequence number in the event stream, so a phone on a bad
connection can sit on a stale view until the user pulls to refresh.

Fix would be to carry `seq` (already the primary key of `assistant_messages`) in
every event and have the app refetch when it sees a jump.

## L6 — foreign keys rely on Colmena's single writer connection

**Partly addressed.** `foreign_keys` is a per-connection pragma, and Colmena
routes every write and transaction through one writer connection, so setting it
once at `Open` covers the write path. That is now *verified* at startup
(`verifyForeignKeys` attempts a violation in a rolled-back transaction and
refuses to boot if it is accepted), so a change in Colmena's pooling becomes a
loud failure instead of silent orphan rows.

What is still not done: putting the pragma in the DSN, which is where it
belongs. That needs a change in Colmena (`store.go` builds the writer DSN) or a
connector wrapper here. Until then the guarantee is asserted rather than
structural.

## L8 — the panel has no CSRF protection

Every mutating panel route is a plain form POST authenticated by the
`takan_session` cookie, with `SameSite=Lax`. Lax stops cross-site *form* posts
in current browsers, which is why this is not urgent, but there is no token: a
future switch to `SameSite=None`, or a browser quirk, would leave the panel open
to a cross-site POST that toggles a module or deletes a vault item.

## L9 — `machine_bash` output is unbounded in the tool result

`machine_ai_status` and `machine_ai_log` cap their tails, but `machine_bash`
returns whatever the command printed. A `cat` of a large file becomes a single
enormous MCP response and, from the assistant, an enormous prompt on the next
turn. The agent-side timeout bounds duration, not size.

## L10 — no metrics endpoint

`/health` reports uptime, poll health, task and job counts. There is nothing
cumulative: no run count over time, no failure rate, no agent duration
histogram. Gatus answers "is it up"; there is no way to answer "is it getting
slower" without reading the journal. `/usage` covers token spend only.
