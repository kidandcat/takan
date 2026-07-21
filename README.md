# Takan

**One MCP connection. Modules for the rest of your life.**

Connect Grok / Claude / Cursor once to Takan. From the web panel, enable modules (Machine, Mercadona, …) — tools appear and disappear without reconfiguring the AI client.

- **Stack:** Go · [Colmena](https://github.com/mentasystems/colmena) (SQLite + continuous backup) · HTMX  
- **Domain (now):** [takan.es](https://takan.es)

## Modules

Integrations live under `modules/` as subpackages (add new ones there):

| Module | Path | Tools | Setup |
|--------|------|--------|--------|
| **Machine** | `modules/machine` | `machine_list`, `machine_bash`, `machine_ai_run`, `machine_ai_status` | Install `takan-agent` on each PC (outbound WSS); Claude/Grok on the machine for AI jobs |
| **Mercadona** | `modules/mercadona` | search, add, list, remove, clear, … | Credentials in panel |
| **Email** | `modules/email` | `email_available_domains`, `email_send`, `email_list`, `email_get`, `email_status` | Resend API key; enable domains in panel |
| **Memory** | `modules/memory` | `memory_get`, `memory_set` | Enable module |
| **People** | `modules/people` | `people_list/get/add/update/delete` | Personal CRM in panel |
| **meta** | `modules` | `takan_status` | Always available |

## MCP

Only the URL is needed. Clients discover OAuth automatically (PKCE), open a browser login, and attach the access token:

```
https://takan.es/mcp
```

## Multi-user

- Accounts are isolated by `user_id` (modules, machines, secrets, people, memory, Mercadona).
- **Invites:** closed registration by default. Users create invite codes (quota `TAKAN_DEFAULT_INVITE_QUOTA`, default 5). Admins can grant **unlimited invites** or change quotas in **Panel → Invites**.
- First registered user becomes **admin + unlimited invites**.
- Mercadona lives in the **main Colmena DB** (same backup as everything else).
- OAuth: PKCE + redirect allowlist; access tokens 24h; refresh rotates (30d).

## Dev

```bash
export TAKAN_PUBLIC_URL=http://127.0.0.1:8090
export TAKAN_SESSION_KEY=$(openssl rand -hex 32)
go run ./cmd/takan
```

## Agent

```bash
go build -o takan-agent ./cmd/takan-agent
./takan-agent --url https://takan.es --token <agent-token> --name mac
```

## License

MIT (product code). Colmena is separate (mentasystems).
