# Runbook: merging the assistant into the hub

One-time cutover. Two services (`takan` and `atlas`) become one, and the
assistant's JSON state moves into the hub's SQLite database.

The order below is not arbitrary: the schema migration must run before the new
binary starts, and the old daemon must be stopped before the new one polls, or
both will call `getUpdates` on the same bot and Telegram will 409 them in turn.

Everything here assumes `vps2`. Nothing in the binary knows a hostname; the two
public hosts come from `ATLAS_PUBLIC_URL` and `ATLAS_APP_URL`.

## 0. Snapshot

```bash
ssh vps2 'sudo systemctl stop takan atlas'
ssh vps2 'sudo cp -a /opt/takan/data /opt/takan/data.bak.$(date +%s)'
ssh vps2 'tar czf ~/atlas-data.bak.$(date +%s).tgz -C /home/debian atlas-data'
ssh vps2 'sudo cp /opt/takan/takan /opt/takan/takan.premerge'
ssh vps2 'sudo cp /usr/local/bin/atlas /usr/local/bin/atlas.premerge'
```

Keep both `.premerge` binaries and both data backups for **14 days**.

## 1. Migrate the schema

Dry-run on a copy first and check the counts at the bottom of
`deploy/migrate-prod.sql` — particularly `vault_items`, which proves the users
collapse kept the right row.

```bash
scp deploy/migrate-prod.sql vps2:/tmp/
ssh vps2 'cp /opt/takan/data/default.db /tmp/dryrun.db && sqlite3 /tmp/dryrun.db < /tmp/migrate-prod.sql'
ssh vps2 'sqlite3 /tmp/dryrun.db "SELECT COUNT(*) FROM users; SELECT COUNT(*) FROM vault_items; PRAGMA foreign_key_check;"'
# only when that looks right:
ssh vps2 'sudo sqlite3 /opt/takan/data/default.db < /tmp/migrate-prod.sql'
```

## 2. Merge the environment

Append to `/etc/takan/takan.env` (values from `/home/debian/atlas.env` and
`/etc/atlas/atlas.env` — do not print them):

```
TELEGRAM_BOT_TOKEN=…            # the one bot; sealed into the DB on first boot
OWNER_TELEGRAM_ID=282611642     # was ALLOWED_CHAT_ID
GROQ_API_KEY=…
ATLAS_APP_TOKEN=…
FIREBASE_SERVICE_ACCOUNT_JSON=…
ATLAS_LOCAL_ADDR=127.0.0.1:8099
ATLAS_APP_URL=https://atlas.jairo.cloud
ATLAS_LEGACY_DIR=/home/debian/atlas-data
```

`ATLAS_SESSION_KEY` **does not change**. Everything sealed with it — the vault,
the Mercadona and email credentials — becomes unreadable under a different key.

`ATLAS_LEGACY_DIR` is what triggers the one-shot import of `state.json`,
`jobs.json`, `tasks.json`, `app-messages.json` and `config.toml`. The importer is
idempotent and renames each file to `.imported` rather than deleting it, so a
retry is safe. Remove the line once the boot log reports the import.

Keep the agent workspace where it is (`/home/debian/atlas-data/workspace`) so
the existing agent sessions and `AGENTS.md` survive: leave `workdir` empty in the
panel only if you have moved the directory, otherwise set it explicitly.

## 3. Stop and mask the old daemon

Do this **before** the new binary starts. Two processes long-polling the same
bot token means neither reliably receives anything.

```bash
ssh vps2 'sudo systemctl stop atlas && sudo systemctl disable atlas && sudo systemctl mask atlas'
ssh vps2 'sudo rm -f /etc/systemd/system/atlas.service.d/takan.conf'
```

Leave `/usr/local/bin/atlas` and the unit file on disk for the rollback.

## 4. Deploy

```bash
deploy/deploy.sh vps2
```

The script removes the old `memory.conf` drop-in (512M would OOM-kill every
agent run now that they share the hub's cgroup), waits for any in-flight
conversation before restarting, and health-checks all three listeners.

## 5. Caddy

Point the app host's `/v1/*` at the hub's port (`127.0.0.1:8090`) instead of the
old daemon's `8099`; see `deploy/Caddyfile.snippet`. The panel host is unchanged.

```bash
ssh vps2 'sudo systemctl reload caddy'
```

## 6. Gatus

- `takan` → unchanged (`https://<panel host>/healthz`)
- `atlas` → unchanged (`http://localhost:8099/health`, now served by the merged
  process). It reports `degraded` with a 503 when polling has been failing for
  over five minutes, which is the case worth paging on: up but deaf.
- `atlas-app` → unchanged (`https://<app host>/v1/health`)

---

## Verification

Panel / auth

- [ ] `curl -fsS https://<panel host>/healthz` → `ok`
- [ ] Sign in with an emailed code
- [ ] Nav shows Assistant under Channels; no Bots, no Telegram, no SIP
- [ ] `/dashboard/bots` and `/dashboard/sip` → 404; `/dashboard/telegram` → redirects to Assistant
- [ ] Vault lists 452 items and one password reveals — proves the session key survived
- [ ] People 44, Health log 13, Machines 5 online

MCP

- [ ] The existing Grok/Claude connector lists tools **without re-authenticating** (proves `oauth_tokens` is intact)
- [ ] `tools/list` has no `bots_*` and no `telegram_chats`
- [ ] `machine_ai_run` no longer requires `owner`
- [ ] `takan_status` returns, with an `assistant` row and no `bots`/`telegram`/`sip`

Assistant

- [ ] DM from Jairo → replies; typing indicator shows
- [ ] DM from a second account → **silence**, one log line, nothing in the panel
- [ ] In a test group: unaddressed message ignored; `@bot ping` from Jairo answered; the same mention from another member **ignored**
- [ ] `/new`, `/status`, `/tasks`, `/cancel`, `/usage` all answer
- [ ] `/usage` shows today / 7d / 30d and a per-model breakdown
- [ ] A >60s request auto-promotes to a task; the notice arrives, the result follows
- [ ] Voice note transcribed; photo lands in `inbox/<chat>/` and the agent opens it

Scheduler / tasks

- [ ] `atlas-sched list` shows the imported jobs with the right `next_run` in Europe/Madrid
- [ ] `atlas-sched add --type message --at "+2m" --text ping` fires on time
- [ ] `atlas-task run "echo hi"` → result in Telegram, `workspace/tasks/<id>/output.txt` exists

App channel

- [ ] `curl -H "Authorization: Bearer $ATLAS_APP_TOKEN" https://<app host>/v1/messages` returns the imported history
- [ ] The app cold-starts with history, and scrolling up loads older messages (`before=`)
- [ ] Send a message → `typing` then `done`; Reset works
- [ ] **A reminder, a routine result and a `/tasks` answer all appear in the app, not only in Telegram** — this is the bug the outbound choke point fixes
- [ ] Close the app, send from Telegram → push arrives

Job round trip

- [ ] `machine_ai_run{machine:"mac", runner:"grok", prompt:"echo roundtrip", chat_id:"282611642"}` → result in the DM **and** in the app history
- [ ] The same without `chat_id` → lands in the DM
- [ ] Break Telegram connectivity, run a job, restore → the 60s sweeper delivers it
- [ ] `SELECT job_id, delivered_at FROM job_chats` shows `delivered_at` set

Ops

- [ ] `systemctl show -p MemoryMax --value takan` ≥ 2G
- [ ] `journalctl -u takan -p warning` clean for 30 minutes
- [ ] `ps -o user= -C grok` → `debian`; `ls -l /home/debian/.grok/auth.json` unchanged
- [ ] `tr '\0' '\n' < /proc/<agent pid>/environ | grep -E 'SESSION_KEY|RESEND|AWS_|APP_TOKEN'` → **empty**
- [ ] `takan-agent` on vps2/vps3/mac still online in `machine_list`

---

## Rollback

The merge changes the schema, so a rollback needs the data backup, not just the
binary.

```bash
ssh vps2 'sudo systemctl stop takan'
ssh vps2 'sudo cp /opt/takan/takan.premerge /opt/takan/takan'
ssh vps2 'sudo rm -rf /opt/takan/data && sudo cp -a /opt/takan/data.bak.<ts> /opt/takan/data'
ssh vps2 'tar xzf ~/atlas-data.bak.<ts>.tgz -C /home/debian'
ssh vps2 'sudo cp /usr/local/bin/atlas.premerge /usr/local/bin/atlas'
ssh vps2 'sudo systemctl unmask atlas && sudo systemctl start takan atlas'
```

Then revert the Caddy block for the app host to `127.0.0.1:8099`.

The `.imported` renames in `/home/debian/atlas-data` are undone by the tarball
restore, so the old daemon finds its state exactly as it left it.

## Risks

| Risk | Mitigation |
|---|---|
| Losing `ATLAS_SESSION_KEY` → 452 vault items unreadable | The key is unchanged. Verify a vault reveal in the panel *before* masking `atlas.service` |
| The old 512M drop-in survives → every agent run OOM-killed | `deploy.sh` removes it; the checklist verifies `MemoryMax` ≥ 2G |
| Two pollers on one bot token (409) | Step 3 stops and masks `atlas` before the new binary starts |
| The importer loses a reminder or a running task | Idempotent and non-destructive (renames, never deletes). Covered by tests over fixtures with the prod shapes. Keep the tarball |
| A restart mid-conversation kills a live agent | `deploy.sh` waits up to 6 minutes for the conversational agent to finish |
| The users collapse cascades something unforeseen | Dry-run on a copy, check the counts, `PRAGMA foreign_key_check` |
| A panel template breaks only when that page is opened | `web.New` parses at startup, and every page is rendered in a test |
