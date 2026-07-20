# Takan

**One MCP connection. Modules for the rest of your life.**

Connect Grok / Claude / Cursor once to Takan. From the web panel, enable modules (Machine, Mercadona, …) — tools appear and disappear without reconfiguring the AI client.

- **Stack:** Go · [Colmena](https://github.com/mentasystems/colmena) (SQLite + continuous backup) · HTMX  
- **Domain (now):** [takan.es](https://takan.es)

## Modules

Integrations live under `modules/` as subpackages (add new ones there):

| Module | Path | Tools | Setup |
|--------|------|--------|--------|
| **Machine** | `modules/machine` | `machine_list`, `machine_bash` | Install `takan-agent` on each PC (outbound WSS) |
| **Mercadona** | `modules/mercadona` | search, add, list, remove, clear, … | Credentials in panel |
| **Email** | `modules/email` | `email_send`, `email_status` | Resend API key + from in panel |
| **Memory** | `modules/memory` | `memory_get`, `memory_set` | Enable module |
| **Files** | `modules/files` | `files_upload`, `files_upload_url`, `files_list`, `files_delete` | Server `TAKAN_FILES_*` (OVH S3) |
| **meta** | `modules` | `takan_status` | Always available |

## MCP

Only the URL is needed. Clients discover OAuth automatically (PKCE), open a browser login, and attach the access token:

```
https://takan.es/mcp
```

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
