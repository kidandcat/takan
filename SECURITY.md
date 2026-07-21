# Security Policy

## Supported versions

Security fixes are applied to the `master` branch of this repository. There are no long-term support releases yet — pin a commit or tag if you need a frozen build.

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security problems.

Report privately via one of:

- GitHub **Security Advisories** on this repository (preferred): *Security → Report a vulnerability*
- Email the maintainer listed on the GitHub profile for [kidandcat](https://github.com/kidandcat)

Include:

- Affected component (MCP, OAuth, panel, `takan-agent`, Mercadona, email, …)
- Impact (auth bypass, cross-tenant read, RCE on hub vs on agent host, secret leak, …)
- Minimal reproduction steps
- Your contact for follow-up

We aim to acknowledge reports within a few days and to ship a fix or mitigation promptly for confirmed issues.

## Threat model (short)

Takan is a **multi-tenant personal hub**. Operators store user secrets (session encryption key, Resend keys, Mercadona credentials, agent tokens).

| Surface | Notes |
|---------|--------|
| **MCP / OAuth** | Bearer access tokens; refresh rotation; redirect URI allowlist. Tool-set changes may force re-auth (401) so clients reload tools. |
| **Web panel** | Session cookie signed with `TAKAN_SESSION_KEY`. Protect this key like a production secret. |
| **Machine agent** | Outbound WSS + `machine_bash` / AI jobs run **on the machine that installed the agent**. Treat agent tokens as root-equivalent for that host. |
| **Mercadona** | Unofficial store API; credentials encrypted at rest with the session key material. See README disclaimer. |
| **Email** | Resend API key encrypted at rest; only user-enabled domains may send/read. |

### Operator checklist

- Generate a long random `TAKAN_SESSION_KEY` (never use the dev default in production).
- Bind the hub to localhost and terminate TLS at a reverse proxy (see `deploy/`).
- Keep registration invite-only unless you accept open self-signup (`TAKAN_ALLOW_REGISTER`).
- Rate limits: `TAKAN_AUTH_PER_MIN`, `TAKAN_MACHINE_BASH_PER_MIN`.
- Do not commit `data/`, `.env`, or production agent tokens.
- Rotate agent tokens if a machine is lost or shared.

## Scope

In scope: authentication/authorization bugs, cross-tenant data access, secret disclosure, remote code execution **on the hub** without intended privileges, and XSS in the panel that can steal sessions.

Out of scope (unless you show impact on the hub multi-tenant boundary): issues that only affect an operator who deliberately installs `takan-agent` and runs AI/shell tools on their own machine; Mercadona or Resend account abuse using the operator’s own credentials; dependency CVEs with no practical exploit path in Takan’s usage.
