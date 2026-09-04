# Takan

## What it is

Personal **MCP hub**: one OAuth connection for AI agents (Grok, Claude, Cursor), and a web panel to enable capabilities (vault, people, health, machines, display, TV, Mercadona, email). Jairo’s life OS control surface, not a marketing site.

Takan is the data and capability layer, not an assistant. The agents are somebody else’s process — Claude Code, Grok, Cursor — and they reach this hub over MCP. The panel exists to configure those capabilities and to approve what needs a human.

## Users

- One operator per instance (Jairo on takan.es; anyone who self-hosts). Panel sign-in is a one-time code emailed to the owner. No passwords, no household accounts, no invites.

## Jobs to be done

- Configure capabilities and see readiness at a glance.
- Manage sensitive data (vault grants, people, health) quickly.
- Approve agent secret grants without leaving the phone later; panel is v1 approve surface.
- Connect agents via a single MCP URL.

## Principles

- **One operator, one instance.** Authorization is the emailed sign-in code and the OAuth token it issues. No accounts, no invites, no approval queue for people.
- **The hub does not run agents, it serves them.** Every capability is an MCP tool an outside agent calls. Nothing here polls, schedules or converses on its own.
- **Secrets leave only on a decision.** Vault reads are grants the operator approves in the panel; the tool waits rather than guessing.
- **A capability with no consumer is deleted.** The panel lists what this binary can actually do, so `takan_status` is trustworthy.

## Mode

**Operate** — task completion, scanability, density, consistent components. Brand is quiet precision, not spectacle.

## Voice

Short, calm, technical when needed. No hype, no “unlock your potential.” Controls name the action.

## Anti-references

AI beige bone gradients · purple SaaS glass · icon-tile marketing grids · nested cards · kicker eyebrows · free unlock of vault secrets in lists.
