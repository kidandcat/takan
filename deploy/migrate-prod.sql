-- One-time migration for the assistant merge.
--
-- Run OFFLINE, on a copy first, with the service stopped:
--
--   sudo systemctl stop takan atlas
--   sudo cp -a /opt/takan/data /opt/takan/data.bak.$(date +%s)
--   # Copy the WAL and shared-memory files too: default.db alone can be missing
--   # the most recent commits, so a dry run on it tests the wrong data.
--   cp /opt/takan/data/default.db* /tmp/ && mv /tmp/default.db /tmp/dryrun.db
--   sqlite3 -bail /tmp/dryrun.db < deploy/migrate-prod.sql   # dry run, check counts
--   sqlite3 -bail /opt/takan/data/default.db < deploy/migrate-prod.sql
--
-- Order matters: this must run BEFORE the new binary starts. The new migrate()
-- no longer creates the dropped tables, and it seeds user_modules rows only for
-- module ids that still exist.
--
-- The new binary drops these tables itself on boot, so this file is really
-- about step 1 (collapsing the users table), which the binary will not do. It
-- is written out in full so the operator can see exactly what changes, and so
-- the row counts can be checked before anything starts.

-- Stop at the first error. Without this the sqlite3 CLI reports a failed
-- statement and CARRIES ON, which would run the DELETEs below even after the
-- owner guard has already said this is the wrong database.
.bail on

PRAGMA foreign_keys = ON;
BEGIN;

-- 0. Refuse to run against the wrong database.
--
-- Everything below hinges on one hardcoded id. If it is absent — wrong host,
-- wrong file, a typo — the DELETEs below would match EVERY row and empty the
-- users table, taking the vault with it via CASCADE.
--
-- Two independent guards, because this is the one irreversible step:
--
--   1. This CHECK, which fails loudly. It needs `.bail on` (above) or
--      `sqlite3 -bail` to stop the script.
--   2. The same condition repeated inside each DELETE's WHERE clause, which
--      needs nothing: on the wrong database they simply match no rows.
CREATE TEMP TABLE owner_guard (n INTEGER CHECK (n = 1));
INSERT INTO owner_guard (n)
  SELECT COUNT(*) FROM users WHERE id = 'e824c2c7-27de-4ed0-875e-0bff55a093bf';

-- 1. Collapse to one user.
--
-- Prod had four rows: the operator, who owns everything, plus three that only
-- ever held two user_modules rows each and some legacy bot placeholders. The
-- panel is single-operator now, so the extras are dead weight that ON DELETE
-- CASCADE will clear out.
--
-- Verify the id first — it is the account that owns the vault, and deleting the
-- wrong row is not recoverable from anything but the backup:
--
--   SELECT id, email FROM users ORDER BY created_at;
--
DELETE FROM user_modules
 WHERE user_id <> 'e824c2c7-27de-4ed0-875e-0bff55a093bf'
   AND (SELECT COUNT(*) FROM users WHERE id = 'e824c2c7-27de-4ed0-875e-0bff55a093bf') = 1;
DELETE FROM users
 WHERE id <> 'e824c2c7-27de-4ed0-875e-0bff55a093bf'
   AND (SELECT COUNT(*) FROM users WHERE id = 'e824c2c7-27de-4ed0-875e-0bff55a093bf') = 1;

-- 2. Drop the retired product surface.
--
-- The bot fleet, the Telegram channel layer, the runtime bundles, invites, the
-- never-written mcp_tokens table, and SIP (wired but never enabled in prod).
DROP TABLE IF EXISTS bot_provision_tickets;
DROP TABLE IF EXISTS bot_deliveries;
DROP TABLE IF EXISTS bot_jobs;
DROP TABLE IF EXISTS bot_chats;
DROP TABLE IF EXISTS bots;
DROP TABLE IF EXISTS telegram_attachments;
DROP TABLE IF EXISTS telegram_channel_chats;
DROP TABLE IF EXISTS telegram_channels;
DROP TABLE IF EXISTS telegram_settings;
DROP TABLE IF EXISTS runtime_bundles;
DROP TABLE IF EXISTS invites;
DROP TABLE IF EXISTS mcp_tokens;
DROP TABLE IF EXISTS sip_devices;
DROP TABLE IF EXISTS sip_settings;

-- 3. Retire the module ids that no longer exist.
DELETE FROM user_modules WHERE module_id IN ('bots','telegram','sip');

DROP TABLE owner_guard;

COMMIT;
VACUUM;

-- Sanity checks. Run these after the migration and before starting the binary;
-- the expected values are from the pre-merge production database.
--
--   SELECT COUNT(*) FROM users;                              -- 1
--   SELECT COUNT(*) FROM machines;                           -- 5
--   SELECT COUNT(*) FROM vault_items;                        -- 452
--   SELECT COUNT(*) FROM people;                             -- 44
--   SELECT client_id, COUNT(*) FROM oauth_tokens GROUP BY 1; -- takan 3, claude-code 1
--   SELECT module_id, enabled FROM user_modules ORDER BY 1;
--   PRAGMA foreign_key_check;                                -- no rows
--
-- The vault count is the one that matters most: it proves the collapse kept the
-- right user. If it is 0, stop and restore the backup.
--
-- NOTE on file_shares: the table exists in prod with 0 rows and has no Go
-- reference anywhere in the tree. It predates the current code and is left
-- alone deliberately — dropping a table nobody can explain is how you find out
-- what wrote it.
