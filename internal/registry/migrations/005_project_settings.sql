-- Per-project cache retention settings, plus fleet defaults.
--
-- Value columns are nullable on purpose: "unset" has to be distinguishable from
-- "set to zero", because zero is a meaningful policy value (TTL 0 = never
-- cache, max_entries 0 = cache nothing). A NULL column falls through to the
-- next level of precedence; a 0 does not.
--
-- Fleet defaults live under the reserved project_id '_fleet'. That cannot
-- collide with a real project: project.ValidateID rejects underscores.
CREATE TABLE IF NOT EXISTS project_settings (
    project_id       TEXT PRIMARY KEY,
    cache_ttl_secs   INTEGER,
    cache_max_entries INTEGER,
    cache_max_bytes  INTEGER,
    updated_at       TEXT NOT NULL,
    updated_by       TEXT
);
