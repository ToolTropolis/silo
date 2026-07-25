package registry

import (
	"context"
	"fmt"
	"time"

	"github.com/rqlite/gorqlite"

	"github.com/tooltropolis/silo/internal/project"
)

var _ TokenStore = (*Rqlite)(nil)

const tokenColumns = `token_hash, project_id, label, created_at, created_by, last_used_at, revoked_at`

func (r *Rqlite) MintToken(ctx context.Context, projectID, label, createdBy string) (string, error) {
	// Validate before minting: a token for a malformed project ID would be a
	// credential that can never authorize anything, which is worse than an error.
	if err := project.ValidateID(projectID); err != nil {
		return "", fmt.Errorf("registry: mint token: %w", err)
	}

	raw, err := NewRawToken()
	if err != nil {
		return "", err
	}

	_, err = r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query: `INSERT INTO agent_tokens (token_hash, project_id, label, created_at, created_by)
			VALUES (?, ?, ?, ?, ?)`,
		Arguments: []interface{}{
			HashToken(raw), projectID, label,
			time.Now().UTC().Format(time.RFC3339), createdBy,
		},
	})
	if err != nil {
		return "", fmt.Errorf("registry: mint token for %q: %w", projectID, err)
	}
	// The only time the raw token exists outside the caller's hand. Nothing
	// logs it, and nothing can recover it from the row just written.
	return raw, nil
}

func (r *Rqlite) VerifyToken(ctx context.Context, rawToken string) (string, error) {
	if rawToken == "" {
		return "", ErrNotFound
	}
	// Look up by hash rather than scanning: the presented token is hashed and
	// matched directly, so the query cost does not grow with the token count
	// and no comparison against stored material is ever needed.
	rows, err := r.conn.QueryOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `SELECT project_id, revoked_at FROM agent_tokens WHERE token_hash = ?`,
		Arguments: []interface{}{HashToken(rawToken)},
	})
	if err != nil {
		return "", fmt.Errorf("registry: verify token: %w", err)
	}
	if !rows.Next() {
		return "", ErrNotFound
	}

	m, err := rows.Map()
	if err != nil {
		return "", fmt.Errorf("registry: verify token: %w", err)
	}
	// A revoked token must be indistinguishable from an unknown one to the
	// caller: both mean "not authorized", and saying which would confirm to an
	// attacker that a token was once valid.
	if revoked, _ := m["revoked_at"].(string); revoked != "" {
		return "", ErrNotFound
	}
	projectID, _ := m["project_id"].(string)
	if projectID == "" {
		return "", ErrNotFound
	}
	return projectID, nil
}

func (r *Rqlite) ListTokens(ctx context.Context, projectID string) ([]AgentToken, error) {
	rows, err := r.conn.QueryOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `SELECT ` + tokenColumns + ` FROM agent_tokens WHERE project_id = ? ORDER BY created_at DESC`,
		Arguments: []interface{}{projectID},
	})
	if err != nil {
		return nil, fmt.Errorf("registry: list tokens for %q: %w", projectID, err)
	}
	var out []AgentToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (r *Rqlite) RevokeToken(ctx context.Context, hash string) error {
	// No RowsAffected check: revoking an already-revoked or unknown token is
	// the caller getting what they asked for. Idempotence matters here because
	// revocation is what you reach for in an incident.
	if _, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `UPDATE agent_tokens SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL`,
		Arguments: []interface{}{time.Now().UTC().Format(time.RFC3339), hash},
	}); err != nil {
		return fmt.Errorf("registry: revoke token: %w", err)
	}
	return nil
}

func (r *Rqlite) RevokeProjectTokens(ctx context.Context, projectID string) (int, error) {
	res, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `UPDATE agent_tokens SET revoked_at = ? WHERE project_id = ? AND revoked_at IS NULL`,
		Arguments: []interface{}{time.Now().UTC().Format(time.RFC3339), projectID},
	})
	if err != nil {
		return 0, fmt.Errorf("registry: revoke tokens for %q: %w", projectID, err)
	}
	return int(res.RowsAffected), nil
}

func (r *Rqlite) TouchToken(ctx context.Context, hash string) error {
	if _, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `UPDATE agent_tokens SET last_used_at = ? WHERE token_hash = ?`,
		Arguments: []interface{}{time.Now().UTC().Format(time.RFC3339), hash},
	}); err != nil {
		return fmt.Errorf("registry: touch token: %w", err)
	}
	return nil
}

func scanToken(rows gorqlite.QueryResult) (AgentToken, error) {
	m, err := rows.Map()
	if err != nil {
		return AgentToken{}, fmt.Errorf("registry: scan token row: %w", err)
	}
	var t AgentToken
	t.Hash, _ = m["token_hash"].(string)
	t.ProjectID, _ = m["project_id"].(string)
	t.Label, _ = m["label"].(string)
	t.CreatedAt, _ = m["created_at"].(string)
	t.CreatedBy, _ = m["created_by"].(string)
	t.LastUsedAt, _ = m["last_used_at"].(string)
	t.RevokedAt, _ = m["revoked_at"].(string)
	return t, nil
}
