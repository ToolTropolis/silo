-- Per-project leader lock. Backed by rqlite (Raft), so acquire/renew/release
-- are linearizable across daemon instances: exactly one owner at a time.
-- Applied idempotently by Rqlite.ensureSchema.

CREATE TABLE IF NOT EXISTS project_locks (
    project_id TEXT PRIMARY KEY,
    owner      TEXT NOT NULL,   -- opaque daemon instance ID
    expires_at INTEGER NOT NULL -- unix epoch seconds, so a dead owner's lock expires
);
