# Takan

**One MCP connection. Modules for the rest of your life.**

Connect Grok, Claude, or Cursor once to Takan. From the web panel, enable modules (Machine, Mercadona, Email, …) — tools appear and disappear without reconfiguring the AI client.

- **Stack:** Go · [Colmena](https://github.com/mentasystems/colmena) (SQLite + continuous backup) · HTMX  
- **Hosted example:** [takan.es](https://takan.es)  
- **License:** [MIT](LICENSE)

## Modules

Integrations live under `modules/` as subpackages:

| Module | Path | Tools | Setup |
|--------|------|--------|--------|
| **Machine** | `modules/machine` | `machine_list`, `machine_bash`, `machine_ai_runners`, `machine_ai_run`, `machine_ai_status` | Install `takan-agent`; AI runners in panel |
| **Mercadona** | `modules/mercadona` | `mercadona_search`, `mercadona_add`, `mercadona_cart` | Credentials in panel |
| **Email** | `modules/email` | `email_available_domains`, `email_send`, `email_list`, `email_get` | Resend API key; enable domains |
| **People** | `modules/people` | `people_list` / `get` / `add` / `update` / `delete` | Personal CRM in panel |
| **Health** | `modules/health` | `health_status`, `health_profile_set`, `health_log_*`, `health_issue_*` | Profile + daily diary + injuries |
| **meta** | `modules` | `takan_status` | Always on — all modules + readiness |

When the tool set changes, Takan may return **401** until the client refreshes OAuth so MCP clients that ignore `tools/list_changed` still reload tools.

## MCP

Only the URL is needed. Clients discover OAuth (PKCE), open a browser login, and attach the access token:

```
https://<your-host>/mcp
```

## Multi-user

- Isolation by `user_id` (modules, machines, secrets, people, health, Mercadona, email).
- **Invites:** registration closed by default. Users create invite codes (`TAKAN_DEFAULT_INVITE_QUOTA`, default 5). Admins can grant unlimited invites in **Panel → Invites**.
- First registered user becomes **admin + unlimited invites**.
- OAuth: PKCE + redirect allowlist; access tokens 24h; refresh rotates (30d).

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

Open the public URL. First user to register is admin.

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
   - Optional: `TAKAN_ALLOW_REGISTER`, invite quota, OAuth extra redirects, rate limits, S3 backup keys

3. **systemd** — see [`deploy/takan.service`](deploy/takan.service) (`EnvironmentFile=…`, `ExecStart=…/takan`).

4. **TLS** — terminate with Caddy/nginx; snippet: [`deploy/Caddyfile.snippet`](deploy/Caddyfile.snippet).

5. **Agent binaries** (optional) — serve under `TAKAN_AGENT_BIN_DIR` (default `/opt/takan/agents`) so `/install.sh` and `/download/takan-agent-<os>-<arch>` work.

6. **Register** the first user on the panel, create machines, enable modules, paste the MCP URL into your AI client.

## Security

See [SECURITY.md](SECURITY.md) for reporting vulnerabilities and a short threat model.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE) for this repository’s product code.

[Colmena](https://github.com/mentasystems/colmena) is a separate project with its own license (mentasystems).
