-- Remove the Atlas assistant layer from a hub database.
--
-- The binary no longer creates or reads these tables, so leaving them behind
-- would only be a stale copy of a conversation. Dropping them is a one-off
-- operator step rather than a boot migration, because it destroys data:
-- SNAPSHOT /opt/takan/data FIRST.
--
--   sudo systemctl stop takan
--   sudo cp -a /opt/takan/data /opt/takan/data.bak.pre-cleanup.$(date +%s)
--   sqlite3 -bail /opt/takan/data/default.db < deploy/cleanup-assistant.sql
--
-- Run it as the user that owns the database (debian), never as root: sqlite3
-- creates -wal/-shm next to the file, and root-owned ones lock the service out.

.bail on

BEGIN;

DROP TABLE IF EXISTS assistant_meta;
DROP TABLE IF EXISTS assistant_chats;
DROP TABLE IF EXISTS assistant_messages;
DROP TABLE IF EXISTS assistant_jobs;
DROP TABLE IF EXISTS assistant_tasks;
DROP TABLE IF EXISTS push_devices;
DROP TABLE IF EXISTS job_chats;

-- The module row too, or takan_status keeps advertising a capability that no
-- longer exists. (The binary also does this on boot; doing it here means the
-- database is already correct before the new binary lands.)
DELETE FROM user_modules WHERE module_id = 'assistant';

COMMIT;

-- Reclaim the pages the conversation log was using.
VACUUM;

-- Sanity counts: everything Takan is FOR must still be here.
SELECT 'users'        AS what, COUNT(*) AS n FROM users
UNION ALL SELECT 'vault_items',  COUNT(*) FROM vault_items
UNION ALL SELECT 'people',       COUNT(*) FROM people
UNION ALL SELECT 'machines',     COUNT(*) FROM machines
UNION ALL SELECT 'oauth_tokens', COUNT(*) FROM oauth_tokens
UNION ALL SELECT 'user_modules', COUNT(*) FROM user_modules;
