# Takan

**One MCP connection. Modules for the rest of your life.**

Connect Grok, Claude, or Cursor once to Takan. From the web panel, enable modules (Machine, Mercadona, Email, …) — tools appear and disappear without reconfiguring the AI client.

- **Stack:** Go · [Colmena](https://github.com/mentasystems/colmena) (SQLite + continuous backup) · HTMX  
- **Hosted example:** [takan.es](https://takan.es) (Hairok’s personal instance — same model as any self-host)  
- **License:** [MIT](LICENSE)

## Modules

Integrations live under `modules/` as subpackages:

| Module | Path | Tools | Setup |
|--------|------|--------|--------|
| **Machine** | `modules/machine` | `machine_list`, `machine_bash` (optional), `machine_ai_runners`, `machine_ai_run`, `machine_ai_status`, `machine_ai_watch`, `machine_ai_log`, `machine_ai_cancel`, `machine_ai_reply` | Install `takan-agent`; toggle bash / AI runners in panel |
| **Display** | `modules/display` | `display_list`, `display_show` | Name a kiosk screen on a machine; agent serves HTML at `127.0.0.1:8787` |
| **Mercadona** | `modules/mercadona` | `mercadona_search`, `mercadona_add`, `mercadona_cart` | Credentials in panel |
| **Email** | `modules/email` | `email_available_domains`, `email_send`, `email_list`, `email_get` | Resend API key; enable domains |
| **People** | `modules/people` | `people_list` / `get` / `add` / `update` / `delete` | Personal CRM in panel |
| **Health** | `modules/health` | `health_status`, `health_log`, `health_issue` | Profile + daily diary + injuries |
| **Telegram** | `modules/telegram` | `telegram_chats`, `telegram_send` | Bot token + allowed chats in panel |
| **SIP** | `modules/sip` | `sip_status`, `sip_devices`, `sip_calls`, `sip_hangup` | xAI key + Android gateways in panel; phones → `wss://…/sip/ws` |
| **Vault** | `modules/vault` | `secrets_search`, `secrets_request`, `secrets_status`, `secrets_store`, `secrets_generate`, `secrets_delete` | Password manager; agent reads require panel approve by default (per-user toggle can auto-approve) |
| **meta** | `modules` | `takan_status` | Always on — all modules + readiness |

When the tool set changes, Takan pushes `notifications/tools/list_changed` on open SSE streams (best-effort). Clients that ignore it keep the old tool list until reconnect; calls to disabled tools simply fail.

`machine_ai_run` returns immediately with a `job_id`. Follow the job with `machine_ai_watch` (blocks until done/failed/cancelled or timeout), `machine_ai_status` (tail), `machine_ai_log` (full transcript), `machine_ai_cancel`, or `machine_ai_reply` (new job with parent context — runners are one-shot and cannot be interrupted in-process). Open SSE streams may also get `notifications/takan/machine_ai_job` when a job ends.

## MCP

Only the URL is needed. Clients discover OAuth (PKCE), open a browser login, and attach the access token:

```
https://<your-host>/mcp
```

OAuth `redirect_uri` accepts any non-empty parseable absolute URI — any scheme (`https`, `cursor://`, RFC 8252 private-use URIs with no host) and any host. Empty or unparseable values are rejected. This hub is personal/single-tenant; there is no redirect host or scheme allowlist.

## Mobile API

JSON REST for the Flutter app (`takan-app`). Bearer access tokens (same store as OAuth).

| Method | Path | Notes |
|--------|------|--------|
| POST | `/api/v1/auth/login` | `{password}` (email ignored) → access + refresh |
| POST | `/api/v1/auth/refresh` | rotate refresh |
| POST | `/api/v1/auth/logout` | revoke access |
| GET | `/api/v1/me` | current user |
| GET | `/api/v1/status` | module readiness |
| GET/POST | `/api/v1/modules` · `…/{id}/toggle` | enable modules |
| GET/POST/DELETE | `/api/v1/vault/…` | items + grants |
| GET/PATCH | `/api/v1/vault/settings` | `{require_approval}` (default true) |
| GET/POST | `/api/v1/approvals` | agent auth inbox (vault grants) |
| GET/POST/DELETE | `/api/v1/people` | directory |
| GET | `/api/v1/health` | snapshot |

Credential reads for agents still use vault grants (`secrets_request` → approve in app or panel, unless the operator turns off “Require approval” in Vault settings).

## Single operator

One process = one operator. There is no signup, invite, or admin/user split. See [TAKAN_SINGLE_OPERATOR.md](TAKAN_SINGLE_OPERATOR.md).

- **Panel:** first visit sets the instance password; afterwards `POST /login` is password-only unlock (httpOnly session cookie, rate-limited).
- **MCP / Grok:** OAuth 2.1 + PKCE + DCR. The browser asks for the same instance password. Tokens still store an internal `user_id` (the owner row) so existing connectors keep working.
- Module tables remain keyed by that owner id. `TAKAN_ALLOW_REGISTER` is ignored.
- OAuth: PKCE; any parseable `redirect_uri`; access tokens 24h; refresh rotates (30d).

## Unofficial Mercadona integration

The Mercadona module talks to the **public web store** (`tienda.mercadona.es`) and the Algolia product index used by that site. **There is no official developer API.**

- Credentials and cart actions run under **your** Mercadona account.
- Behaviour can break without notice if Mercadona changes the site or auth.
- Rate limits, account lockouts, and compliance with Mercadona’s terms are **your responsibility** as the operator and end user.
- Algolia app id/key in source are the same values the browser SPA embeds for anonymous search; they rotate occasionally.

This project is not affiliated with or endorsed by Mercadona.

## Development

```bash
git clone https://github.com/kidandcat/takan.git
cd takan
export TAKAN_PUBLIC_URL=http://127.0.0.1:8090
export TAKAN_SESSION_KEY=$(openssl rand -hex 32)
go test ./...
go run ./cmd/takan
```

Open the public URL. First visit sets the instance password (bind to localhost until then).

### Agent (local)

```bash
go build -o takan-agent ./cmd/takan-agent
./takan-agent --url http://127.0.0.1:8090 --token <agent-token> --name mac
```

Create a machine in the panel to get the install one-liner / token.

## Self-hosting (production sketch)

1. **Build**

   ```bash
   CGO_ENABLED=0 go build -o takan ./cmd/takan
   CGO_ENABLED=0 go build -o takan-agent ./cmd/takan-agent
   # optional multi-arch agents for /download/:
   # GOOS=linux GOARCH=amd64 go build -o takan-agent-linux-amd64 ./cmd/takan-agent
   ```

2. **Config** — copy [`deploy/takan.env.example`](deploy/takan.env.example) to e.g. `/etc/takan/takan.env`:

   - `TAKAN_PUBLIC_URL=https://your.domain` (must match what clients use)
   - `TAKAN_SESSION_KEY=` long random (`openssl rand -hex 32`) — **never** the dev default
   - `TAKAN_DATA_DIR=` writable path for Colmena/SQLite
   - `TAKAN_LISTEN=127.0.0.1:8090` (prefer reverse-proxy TLS)
   - Optional: rate limits, S3 backup keys (`TAKAN_ALLOW_REGISTER` is ignored)

3. **systemd** — see [`deploy/takan.service`](deploy/takan.service) (`EnvironmentFile=…`, `ExecStart=…/takan`).

4. **TLS** — terminate with Caddy/nginx; snippet: [`deploy/Caddyfile.snippet`](deploy/Caddyfile.snippet).

5. **Agent binaries** (optional) — serve under `TAKAN_AGENT_BIN_DIR` (default `/opt/takan/agents`) so `/install.sh` and `/download/takan-agent-<os>-<arch>` work.

6. **Set the instance password** on the panel, create machines, enable modules, paste the MCP URL into your AI client.

## Security

See [SECURITY.md](SECURITY.md) for reporting vulnerabilities and a short threat model.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE) for this repository’s product code.

[Colmena](https://github.com/mentasystems/colmena) is a separate project with its own license (mentasystems).
