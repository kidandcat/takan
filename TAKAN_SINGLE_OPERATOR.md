# Takan: one operator = one instance

Decision (Hairok / kidandcat, 2026-08-21): **takan.es is a personal self-host**, same as any other. It stays behind a strong instance secret. There are no accounts, invites, or viral signup.

This document is the design. The code in this branch implements the **safe application-layer cut**. Schema collapse is specified here and **must not be executed on production SQLite** (`/opt/takan/data`).

## Model

| Concept | Meaning |
|---------|---------|
| Instance | One Takan process + one Colmena/SQLite directory |
| Operator | The person who runs that process |
| Instance password | bcrypt hash on the **owner** row (`users.password_hash`) |
| Panel unlock | `POST /login` with that password → `web_sessions` cookie (`takan_session`, httpOnly, SameSite=Lax, Secure on https) |
| MCP / Grok | OAuth 2.1 + PKCE (`/oauth/authorize`, `/oauth/token`, DCR `POST /oauth/register`). Tokens keep a `user_id` column internally; that is not a product “account” |
| Agents | `machines.agent_token_hash` / SIP device tokens — unchanged |

There is **no** register, invite code, invite quota, admin-vs-user role, or `TAKAN_ALLOW_REGISTER` product switch. Env vars for those are ignored if still present.

Rate limits stay (`TAKAN_AUTH_PER_MIN` on login / OAuth token / API login).

## What this PR deletes (HTTP / UI)

Removed from the product surface (404):

| Path | Was |
|------|-----|
| `GET/POST /register` | Email + password + invite signup (`internal/web/web.go` `registerGet`/`registerPost`, `templates/register.html`) |
| `GET /dashboard/invites` + create/revoke POSTs | Invite minting (`templates/invites.html`) |
| `GET /dashboard/admin` + `POST /dashboard/admin/users` | User table + `is_admin` / quota (`templates/admin.html`) |
| `GET/POST /api/v1/invites` | Mobile invite CRUD (`internal/api/resources.go`) |
| Landing / login / OAuth “Register with invite” CTAs | `home.html`, `login.html`, `oauth/html.go` |
| Nav: Invites, Admin | `templates/layout.html` |

`TAKAN_ALLOW_REGISTER` and `TAKAN_DEFAULT_INVITE_QUOTA` are **not read**. Setting them true/5 does nothing.

Store helpers in `internal/store/invites.go` and columns `users.invite_quota` / `invite_unlimited` / `is_admin` remain **in SQLite** so this binary can boot an existing database without `ALTER`/`DROP`. HTTP no longer calls them.

## What stays

- **Instance password** — owner bcrypt; min 8 characters (same rule as `CreateUser`).
- **Web session** — `web_sessions.token` → owner `user_id` only. A leftover session for a non-owner row is treated as logged out.
- **MCP OAuth + DCR** — `oauth_codes`, `oauth_tokens`, `oauth_refresh`; public client id `takan`; PKCE S256; any parseable `redirect_uri` (see `internal/oauth/redirect.go`).
- **Mobile API login** — `POST /api/v1/auth/login` with `{password}` (email field ignored if sent). Issues the same OAuth access/refresh rows (`client=takan-app`).
- **Agent / SIP tokens** — hashed in `machines` / `sip_devices`.
- **Module rows keyed by `user_id`** — still the isolation column in SQLite. The operator’s data is the **owner** id. Extra historical rows (if any) stay put until a future collapse.

`mcp_tokens` is already unused (created in `migrate()` never written). Leave it.

## How Grok Bot authenticates without a “user”

Grok (and Claude / Cursor) never had a Takan username API. They already do:

1. MCP URL `https://<host>/mcp`
2. RFC 9728 resource metadata + AS metadata
3. Optional DCR `POST /oauth/register` → `{client_id: "takan", token_endpoint_auth_method: "none"}`
4. Browser `GET /oauth/authorize` with PKCE `code_challenge`
5. Operator types the **instance password** (no email)
6. Consent Allow → redirect with `code` → `POST /oauth/token` (code + verifier)
7. `Authorization: Bearer` on `/mcp`; refresh rotates (`internal/store.RotateRefreshToken`)

Internally the access token row still has `user_id = owner.id`. `mcp.Server.Resolve` (`cmd/takan/main.go`) calls `UserByAccessToken` and tools load modules for that id. That is a storage key, not a second person.

**Existing Grok connection on takan.es** (today bound to `kidandcat@gmail.com`’s user id): refresh and access rows are **not rewritten**. `UserByAccessToken` still returns that row. Re-auth in the browser uses the instance password (the owner’s current bcrypt, i.e. that account’s password).

Do **not** delete the owner `users` row. `oauth_tokens.user_id` is `REFERENCES users(id) ON DELETE CASCADE` — dropping that user deletes Grok’s tokens.

## Owner selection (no schema change)

```
SELECT id FROM users
ORDER BY is_admin DESC, created_at ASC, id ASC
LIMIT 1
```

- Empty DB → `POST /login` **bootstraps** one row: email sentinel `operator@local` (`store.OperatorEmail`), `is_admin=1`, password hash of the form value. This is the only write to `users` from HTTP.
- Existing DB (takan.es) → earliest admin, else earliest user. Extra rows **cannot** unlock the panel, OAuth login, or API login. Their **already-issued** OAuth/agent tokens still resolve.

OAuth **does not** bootstrap. If there is no owner yet, authorize tells the operator to set the password at `/login` first (avoids a race on a public `/oauth/authorize`).

Panel **does** bootstrap on first `POST /login` (same first-arriver risk as the old first-register-is-admin). Bind `TAKAN_LISTEN` to localhost until the password exists.

## Password change

`GET /dashboard/instance` + `POST /dashboard/instance/password` (`current`, `new`). Updates owner `password_hash`, **deletes `web_sessions` for that id** (panel cookies die). Does **not** revoke `oauth_tokens` / `oauth_refresh` (would kick Grok).

## Future schema migration (do **not** run on prod)

Goal: one `users` row, extra people gone, every FK pointing at the owner, tokens still valid.

**Do not ship this as an automatic `migrate()`.** Production currently has multiple `users` rows. Unique indexes will collide if two people have the same machine/display/SIP name. Mercadona `accounts.id` **is** the user id (no FK, no CASCADE). A blind `UPDATE user_id` + `DELETE FROM users` is how you lose Grok and vault data.

### Inventory (every `REFERENCES users(id)` plus Mercadona)

From `internal/store` on master:

| Table | Column | On delete | Collapse hazard |
|-------|--------|-----------|-----------------|
| `web_sessions` | `user_id` | CASCADE | Re-point or delete extras |
| `mcp_tokens` | `user_id` | CASCADE | Unused; delete extras |
| `user_modules` | `user_id` | CASCADE | PK `(user_id, module_id)` — `INSERT OR IGNORE` owner row, then delete extras |
| `machines` | `user_id` | CASCADE | `UNIQUE(user_id, name)` — rename extras (`name || '-' || substr(old_id,1,8)`) before re-point |
| `mercadona_creds` | `user_id` PK | CASCADE | Keep owner row; drop extras (or refuse if extra has data operator wants) |
| `oauth_codes` | `user_id` | CASCADE | Re-point (or let expire) |
| `oauth_tokens` | `user_id` | CASCADE | **Re-point to owner** — this is Grok/Claude/Cursor |
| `oauth_refresh` | `user_id` | CASCADE | **Re-point to owner** |
| `email_settings` | `user_id` PK | CASCADE | Keep owner |
| `telegram_settings` | `user_id` PK | CASCADE | Keep owner |
| `people` | `user_id` | CASCADE | Re-point |
| `invites` | `created_by`, `used_by` | CASCADE / SET NULL | Drop table |
| `health_profile` | `user_id` PK | CASCADE | Keep owner |
| `health_log` | `user_id` | CASCADE | `UNIQUE(user_id, day)` — merge or skip duplicate days |
| `health_issues` | `user_id` | CASCADE | Re-point |
| `vault_items` | `user_id` | CASCADE | Re-point |
| `vault_grants` | `user_id` | CASCADE | Re-point |
| `vault_devices` | `user_id` | CASCADE | `UNIQUE(user_id, name)` — rename |
| `vault_audit` | `user_id` | CASCADE | Re-point |
| `sip_settings` | `user_id` PK | CASCADE | Keep owner |
| `sip_devices` | `user_id` | CASCADE | `UNIQUE(user_id, name)` — rename |
| `displays` | `user_id` | CASCADE | `UNIQUE(user_id, name)` — rename |
| `accounts` (Mercadona) | `id` **is** user id, **no FK** | n/a | `UPDATE accounts SET id = owner WHERE id = extra` will collide if both exist — keep owner, drop extra only after review |
| `grocery_*` | `account_id` | n/a | Follow accounts |

### Procedure (manual, off-prod copy first)

1. Copy SQLite. Never run on `/opt/takan/data` in place without a Colmena restore drill.
2. `owner_id :=` same `ORDER BY is_admin DESC, created_at ASC, id ASC LIMIT 1` (expect Hairok’s existing id, email `kidandcat@gmail.com`).
3. Print extra user ids and row counts per table. Stop if any extra has machines/vault/oauth you still need merged.
4. Rename unique-name collisions on extras.
5. `UPDATE … SET user_id = owner_id WHERE user_id != owner_id` (and Mercadona `accounts.id` / `grocery_*.account_id` only if merging that extra’s grocery).
6. `DELETE FROM users WHERE id != owner_id` — CASCADE then only hits leftovers.
7. `DROP TABLE IF EXISTS invites;`
8. Optional later: drop `invite_quota`, `invite_unlimited`, `is_admin` (requires table rebuild in SQLite).
9. Confirm `SELECT user_id, client_id, COUNT(*) FROM oauth_tokens GROUP BY 1,2` still has Grok’s client rows on `owner_id`.
10. Restart hub, `curl /healthz`, Grok connector still lists tools.

Until that happens, **this binary is correct on the live DB**: one unlock password (owner hash), extra rows inert for panel, tokens still valid.

## Tests (this branch)

- `store`: owner pick, bootstrap once, extra-user password cannot authenticate, extra-user **token** still `UserByAccessToken`s, password change wipes web sessions only.
- `web`: first POST sets password; second user register 404; invites 404; extra-user cookie rejected; unlock + dashboard.
- `oauth`: authorize login is password-only; register link absent; token still bound to owner id.
- `api`: login `{password}`; `{email,password}` still works if password is the owner’s; `/api/v1/invites` 404.

Existing `tenant_test.go` / vault isolation tests still create two `users` rows via `CreateUserOpts` (fixture helper, not HTTP). That is leftover storage isolation, not a product feature.

## Production

**Do not deploy this to takan.es from this PR. Do not open `/opt/takan/data`. Do not run the collapse SQL.**

Live hub can keep serving the previous binary until an explicit deploy order. After a future deploy of *this* application layer (still no DROP): Hairok unlocks with the existing password for `kidandcat@gmail.com`; Grok’s refresh token keeps working.
