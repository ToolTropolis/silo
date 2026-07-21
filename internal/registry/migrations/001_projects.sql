-- Tenant registry schema. Applied idempotently by Rqlite.ensureSchema on first
-- use. rqlite speaks SQLite, so this is plain SQLite DDL.

CREATE TABLE IF NOT EXISTS projects (
    project_id    TEXT PRIMARY KEY,
    bucket_name   TEXT NOT NULL,
    credential_id TEXT NOT NULL DEFAULT '',
    key_id        TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,               -- RFC3339
    status        TEXT NOT NULL                -- active | decommissioning | decommissioned
);

CREATE INDEX IF NOT EXISTS idx_projects_status ON projects (status);
