-- Agent tokens: the bearer credentials that scope an agent to one project.
--
-- Only a HASH of each token is stored, never the token itself. A token is a
-- live credential — anyone holding it can read and write that project's memory
-- — so a leaked registry dump must not hand an attacker working access. This
-- is the same reason password databases store hashes, and it is why the console
-- can show a token exactly once, at creation, and never again.
--
-- The hash is SHA-256 over the raw token. Deliberately not bcrypt/argon2:
-- those are designed to be slow, and this hash is computed on the authorization
-- path of every agent request. Tokens are 256 bits of CSPRNG output rather than
-- human-chosen passwords, so there is no dictionary to attack and no work
-- factor to add — the entropy does the job that slowness does for passwords.
CREATE TABLE IF NOT EXISTS agent_tokens (
    -- token_hash is the primary key: lookup is by hash, so a token presented by
    -- an agent is hashed and matched directly rather than scanned for.
    token_hash  TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL,
    -- label distinguishes several tokens for one project ("laptop", "ci"),
    -- so one can be revoked without disturbing the others.
    label       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    created_by  TEXT,
    -- last_used_at is best-effort: it makes an unused token identifiable before
    -- revoking it. Updated lazily, never on the read path's critical section.
    last_used_at TEXT,
    -- revoked_at set means the token is dead. Kept rather than deleted so an
    -- audit can still answer "what happened to that token?".
    revoked_at  TEXT
);

-- Every authorization does a lookup by hash filtered on revocation, and the
-- console lists a project's tokens.
CREATE INDEX IF NOT EXISTS idx_agent_tokens_project ON agent_tokens (project_id);
