# Takan

## What it is

Personal **MCP hub and assistant**: one OAuth connection for AI agents (Grok, Claude, Cursor), a web panel to enable capabilities (vault, people, health, machines, display, email, …), and one Telegram bot plus a phone app that answer only the operator. Jairo’s life OS control surface, not a marketing site.

Two shapes of the same thing:

- **Capabilities** are what other agents reach through MCP.
- **Channels** are how the operator reaches his own assistant: Telegram, the phone app, and email.

One process, one database, one credential set. The assistant used to be a second service with its own state files and a REST outbox between them; it is not any more.

## Users

- One operator per instance (Jairo on takan.es; anyone who self-hosts). Panel sign-in is a one-time code emailed to the owner. No passwords, no household accounts, no invites.

## Jobs to be done

- Ask the assistant for something from the phone and get an answer, or a task that reports back.
- Configure capabilities and see readiness at a glance.
- Manage sensitive data (vault grants, people, health) quickly.
- Approve agent secret grants without leaving the phone later; panel is v1 approve surface.
- Connect agents via a single MCP URL.
- Know what the agent is costing: `/usage` reports today / 7d / 30d.

## Principles

- **The assistant answers one person.** Authorization is identity, not chat: a stranger gets silence, never a refusal, so the bot does not confirm its own existence. There is no whitelist to maintain and no approval queue.
- **Never make the operator wait.** A reply over the conversational budget is promoted to a background task rather than killed; the chat is freed and the result arrives later.
- **Telegram and the app are one conversation.** Everything the assistant says reaches both, so the phone never shows a version of the day with holes in it.
- **The agent is not trusted with the hub's secrets.** It runs model-authored commands, so it gets an environment allowlist.

## Mode

**Operate** — task completion, scanability, density, consistent components. Brand is quiet precision, not spectacle.

## Voice

Short, calm, technical when needed. No hype, no “unlock your potential.” Controls name the action.

## Anti-references

AI beige bone gradients · purple SaaS glass · icon-tile marketing grids · nested cards · kicker eyebrows · free unlock of vault secrets in lists.
