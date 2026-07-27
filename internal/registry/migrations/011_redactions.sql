-- Audit trail for redacted object versions.
--
-- A secret written into memory is in every S3 object version forever, and the
-- only remedy was destroying the whole project. Redaction removes one version's
-- content without touching the rest of the history.
--
-- SeaweedFS can delete a specific version, but only as a HARD delete: the
-- version record disappears with the bytes. Verified against the dev cluster —
-- deleting one versionId left the other versions intact and readable, and the
-- deleted one vanished from ListVersions entirely. There is no native way to
-- blank a version's content while keeping its record.
--
-- So the record is kept HERE instead. The bytes are genuinely destroyed, which
-- is the point when a credential leaks — a tombstone the object store still
-- holds is not erasure — and the audit of who removed what, when, and why lives
-- in rqlite where deleting the object cannot also delete the evidence.
--
-- Rows are never deleted. A redaction that could itself be redacted is not an
-- audit trail.
CREATE TABLE IF NOT EXISTS redactions (
    -- The version that was destroyed. Not a foreign key to anything: the object
    -- it names is gone by the time this row is durable, which is precisely why
    -- the row has to exist.
    project_id  TEXT NOT NULL,
    path        TEXT NOT NULL,
    version_id  TEXT NOT NULL,
    -- Why it was removed. Required by the console rather than by the schema,
    -- since an operator reading this in six months needs the reason more than
    -- the timestamp.
    reason      TEXT NOT NULL DEFAULT '',
    redacted_at TEXT NOT NULL,
    redacted_by TEXT,
    PRIMARY KEY (project_id, path, version_id)
);

-- The console lists a project's redactions, and the memory browser marks
-- redacted versions inline while rendering one path's history.
CREATE INDEX IF NOT EXISTS idx_redactions_project_path
    ON redactions (project_id, path);
