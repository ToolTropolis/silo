-- Ledger of applied migrations.
--
-- Until now idempotency came from every migration being CREATE TABLE IF NOT
-- EXISTS, so re-running the whole set was harmless. That breaks the moment a
-- migration alters an existing table: ALTER TABLE has no IF NOT EXISTS, so the
-- second run fails with "duplicate column name" and takes the daemon's startup
-- with it.
--
-- Recording what has run lets later migrations be genuinely one-shot.
CREATE TABLE IF NOT EXISTS schema_migrations (
    name       TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL              -- RFC3339
);
