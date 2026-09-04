# Follow-ups

Known gaps in the hub, with the reasoning for leaving them. Each one was looked
at and deliberately deferred; none is a placeholder for "we forgot".

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
enormous MCP response, and therefore an enormous prompt on the calling agent's
next turn. The agent-side timeout bounds duration, not size.
