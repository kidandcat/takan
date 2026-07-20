# Takan

**One MCP connection. Modules for the rest of your life.**

Connect Grok / Claude / Cursor once to Takan. From the web panel, enable modules (Machine, Mercadona, …) — tools appear and disappear without reconfiguring the AI client.

- **Stack:** Go · [Colmena](https://github.com/mentasystems/colmena) (SQLite + continuous backup) · HTMX  
- **Domain (now):** [takan.es](https://takan.es)

## Modules

| Module | Tools | Setup |
|--------|--------|--------|
| **Machine** | `machine_list`, `machine_bash` | Install `takan-agent` on each PC (outbound WSS) |
| **Mercadona** | `mercadona_status` (+ cart next) | Credentials in panel |
| **meta** | `takan_status` | Always available |

## MCP

```
POST https://takan.es/mcp
Authorization: Bearer <token from panel>
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
