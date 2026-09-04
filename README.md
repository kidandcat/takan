# Takan

**One MCP connection. Modules for the rest of your life.**

Connect Grok, Claude, or Cursor once to Takan. From the web panel, enable modules (Machine, Mercadona, Email, …) — tools appear and disappear without reconfiguring the AI client.

It also runs your **assistant**: one Telegram bot and a phone app, answering only you, driven by a CLI coding agent. Same process, same database, same credentials.

- **Stack:** Go · [Colmena](https://github.com/mentasystems/colmena) (SQLite + continuous backup) · HTMX  
- **Self-host:** this repo — one process, your machine. See [Docker Compose](#self-hosting) below.  
- **Hosted example:** [takan.es](https://takan.es) (Hairok’s personal instance — same model as any self-host)  
- **License:** [MIT](LICENSE)

## Modules

Integrations live under `modules/` as subpackages:

| Module | Path | Tools | Setup |
|--------|------|--------|--------|
| **Machine** | `modules/machine` | `machine_list`, `machine_bash` (optional), `machine_ai_runners`, `machine_ai_run`, `machine_ai_status`, `machine_ai_watch`, `machine_ai_log`, `machine_ai_cancel`, `machine_ai_reply` | Install `takan-agent`; toggle bash / AI runners in panel |
| **Assistant** | `modules/assistant` | `telegram_send` | Your personal assistant: one Telegram bot, the phone app channel, background tasks and scheduled routines. `TELEGRAM_BOT_TOKEN` + `OWNER_TELEGRAM_ID` in the env; the runner is configured in the panel |
| **Display** | `modules/display` | `display_list`, `display_show` | Name a kiosk screen on a machine; agent serves HTML at `127.0.0.1:8787` |
| **TV** | `modules/tv` | `tv_status`, `tv_app`, `tv_key`, `tv_text`, `tv_volume`, `tv_mute`, `tv_power`, `tv_now` | Samsung Tizen on the LAN; hub relays short curl / UPnP / samsungtvws / WOL commands to a takan-agent (panel: machine, host, token path, wifi MAC, app aliases) |
| **Mercadona** | `modules/mercadona` | `mercadona_search`, `mercadona_add`, `mercadona_cart` | Credentials in panel |
| **Email** | `modules/email` | `email_available_domains`, `email_send`, `email_list`, `email_get` | Resend API key; enable domains |
| **People** | `modules/people` | `people_list` / `get` / `add` / `update` / `delete` | Personal CRM in panel |
| **Health** | `modules/health` | `health_status`, `health_log`, `health_issue` | Profile + daily diary + injuries |
| **Vault** | `modules/vault` | `secrets_search`, `secrets_request`, `secrets_status`, `secrets_store`, `secrets_generate`, `secrets_delete` | Password manager; agent reads require panel approve by default (per-user toggle can auto-approve) |
| **meta** | `modules` | `takan_status` | Always on — all modules + readiness |

When the tool set changes, Takan pushes `notifications/tools/list_changed` on open SSE streams (best-effort). Clients that ignore it keep the old tool list until reconnect; calls to disabled tools simply fail.

`machine_ai_run` returns immediately with a `job_id`; optional `chat_id` names the Telegram chat that gets the result (default: your own). Follow the job with `machine_ai_watch` (blocks until done/failed/cancelled or timeout), `machine_ai_status` (tail), `machine_ai_log` (full transcript), `machine_ai_cancel`, or `machine_ai_reply` (a new job with parent context — runners are one-shot and cannot be interrupted in-process). Open SSE streams may also get `notifications/takan/machine_ai_job` when a job ends.

When a job finishes, the assistant delivers the result to that chat and to your phone app. Routing lives in one `job_chats` row per job; an undelivered result is retried every 60s for an hour. There is no bot registry and no outbox — `machine_ai_run` no longer takes an `owner`, and an `owner` sent by an older client is ignored.

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
| POST | `/api/v1/auth/send-code` | emails a one-time code to the owner |
| POST | `/api/v1/auth/login` | `{code}` (email ignored) → access + refresh |
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

## Assistant

One bot, one operator, one conversation — reachable from Telegram and from the phone app, which share the same history and the same agent session.

**Authorization is identity, not chat.** `OWNER_TELEGRAM_ID` is your Telegram *user* id. The assistant answers that id and nothing else: a stranger who DMs the bot gets silence, not a refusal, and in a group only your own messages are read — everyone else's are dropped before they can reach the prompt. Adding the bot to a group therefore needs no approval step. (Known limitation: a message posted *anonymously* as a group admin arrives from Telegram's `GroupAnonymousBot`, so it is ignored.)

**Replies come from a CLI coding agent**, `grok` by default, configured in the panel. A reply still running after ~60s is not killed: it is promoted to a background task, the chat is freed, and the result arrives when it is ready. The agent inherits an environment *allowlist*, never the hub's own — it cannot see the session key, the mail key or the backup credentials.

Telegram commands: `/new`, `/cancel`, `/tasks`, `/usage`, `/status`, `/help`.

Three helper binaries (symlinks to the same binary, dispatching on `argv[0]`) let the agent reach you and own the clock:

```bash
atlas-send "the backup finished"
atlas-send --file chart.png "monthly numbers"
atlas-task run "build the release and report" --title "release build"
atlas-sched add --type message --at "+90m" --text "take the bread out"
atlas-sched add --type agent --cron "30 7 * * *" --name morning --text "check disk usage"
```

They talk to the loopback API (`ATLAS_LOCAL_ADDR`), so they carry no credentials of their own.

Every message the assistant sends you goes through one exit point, so the phone app's conversation and Telegram never disagree — reminders, routine output, command answers and job results all land in both.

## Future: browser capability

The assistant can drive a browser, but through the agent rather than through Takan: [`mcp-chrome`](https://github.com/hangwin/mcp-chrome) is a Chrome extension plus an MCP server that exposes the *running* browser — your real profile, already logged in — to the agent. Add it to the agent's own MCP config (`~/.grok/config.toml`), not here.

That is deliberately the whole design. Takan stores no browser cookies and owns no profile: the sessions stay in Chrome where they already are, so there is nothing extra to encrypt, expire, or leak. Nothing in this repo needs to change to enable it.

## Single operator

One process = one operator. There is no signup, invite, or admin/user split. See [TAKAN_SINGLE_OPERATOR.md](TAKAN_SINGLE_OPERATOR.md).

- **Panel:** sign-in is a 6-digit one-time code emailed to `TAKAN_OWNER_EMAIL` (single-use, 10 min, rate-limited). There is no password. The first verified code also creates the owner.
- **MCP / Grok:** OAuth 2.1 + PKCE + DCR. `/oauth/authorize` has no credential form: it bounces to `/login` and resumes consent afterwards. Tokens still store an internal `user_id` (the owner row) so existing connectors keep working.
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

Set `TAKAN_OWNER_EMAIL`, open the public URL and press **Send code**. The first verified code creates the owner.

### Agent (local)

```bash
go build -o takan-agent ./cmd/takan-agent
./takan-agent --url http://127.0.0.1:8090 --token <agent-token> --name mac
```

Create a machine in the panel to get the install one-liner / token.

## Self-hosting

Takan is a **single-operator personal hub**: you run it, and login codes go to your address. It is not a multi-tenant SaaS. There is no signup or invites (`TAKAN_ALLOW_REGISTER` is ignored). See [TAKAN_SINGLE_OPERATOR.md](TAKAN_SINGLE_OPERATOR.md).

### Docker Compose (recommended)

```bash
git clone https://github.com/kidandcat/takan.git
cd takan
docker compose up --build
```

Open http://localhost:8090 and sign in with an emailed code. Create a machine in the panel, then on each PC:

```bash
curl -fsSL http://localhost:8090/install.sh | bash -s -- <agent-token>
```

MCP URL for Grok / Claude / Cursor: `http://localhost:8090/mcp`.

- Data (SQLite + generated `TAKAN_SESSION_KEY`) lives in the `takan-data` volume — keep it. Losing it loses vault ciphertext.
- Other devices on the LAN: `TAKAN_PUBLIC_URL=http://192.168.x.x:8090 docker compose up --build` (must be the URL those clients actually open).
- Behind TLS: set `TAKAN_PUBLIC_URL=https://takan.example.com` and reverse-proxy port 8090 (see [`deploy/Caddyfile.snippet`](deploy/Caddyfile.snippet)).

### systemd (bare metal)

1. **Build**

   ```bash
   CGO_ENABLED=0 go build -o takan ./cmd/takan
   CGO_ENABLED=0 go build -o takan-agent ./cmd/takan-agent
   # multi-arch agents for /download/ and /install.sh:
   # GOOS=linux GOARCH=amd64 go build -o takan-agent-linux-amd64 ./cmd/takan-agent
   ```

2. **Config** — copy [`deploy/takan.env.example`](deploy/takan.env.example) to `/etc/takan/takan.env`:

   - `TAKAN_PUBLIC_URL=https://takan.example.com` (must match what clients use)
   - `TAKAN_SESSION_KEY=` long random (`openssl rand -hex 32`) — **never** the dev default
   - `TAKAN_DATA_DIR=` writable path for Colmena/SQLite
   - `TAKAN_LISTEN=127.0.0.1:8090` (prefer reverse-proxy TLS)
   - `TELEGRAM_BOT_TOKEN` + `OWNER_TELEGRAM_ID` to enable the assistant
   - Optional: rate limits, S3-compatible backup keys (`TAKAN_ALLOW_REGISTER` is ignored)

   Every `TAKAN_*` name is also accepted as `ATLAS_*`, which is the preferred spelling; the old name still works and logs a deprecation line once.

3. **systemd** — [`deploy/takan.service`](deploy/takan.service). It runs as an ordinary login user with a real `HOME`: the CLI agent the assistant spawns needs its own credentials (`~/.grok/auth.json`), and running as that user is what makes them readable without copying anything. `MemoryMax=2G` covers the agent subprocesses, which share the cgroup.

4. **TLS** — Caddy/nginx; snippet: [`deploy/Caddyfile.snippet`](deploy/Caddyfile.snippet).

5. **Agent binaries** — put `takan-agent-<os>-<arch>` in `TAKAN_AGENT_BIN_DIR` (default `/opt/takan/agents`) so `/install.sh` works. The Docker image already includes linux/darwin amd64+arm64.

6. **Sign in with an emailed code** on the panel, create machines, enable modules, paste the MCP URL into your AI client.

OSS packaging notes (what is in / out of a hosted SaaS): [TAKAN_OSS_SELFHOST.md](TAKAN_OSS_SELFHOST.md).

## Security

See [SECURITY.md](SECURITY.md) for reporting vulnerabilities and a short threat model.
Known gaps, with the reasoning for leaving them open: [docs/FOLLOWUPS.md](docs/FOLLOWUPS.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE) for this repository’s product code.

[Colmena](https://github.com/mentasystems/colmena) is a separate project with its own license (mentasystems).
