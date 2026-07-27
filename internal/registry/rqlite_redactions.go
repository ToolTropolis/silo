package registry

import (
	"context"
	"fmt"
	"time"

	"github.com/rqlite/gorqlite"
)

var _ RedactionStore = (*Rqlite)(nil)

const redactionColumns = `project_id, path, version_id, reason, redacted_at, redacted_by`

func (r *Rqlite) RecordRedaction(ctx context.Context, red Redaction) error {
	redactedAt := red.RedactedAt
	if redactedAt == "" {
		redactedAt = time.Now().UTC().Format(time.RFC3339)
	}
	// ON CONFLICT DO NOTHING rather than an upsert: the first record of a
	// redaction is the true one. Re-recording the same version would let a
	// later caller rewrite the reason or the actor on an existing audit row.
	_, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query: `INSERT INTO redactions (` + redactionColumns + `)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_id, path, version_id) DO NOTHING`,
		Arguments: []interface{}{
			red.ProjectID, red.Path, red.VersionID, red.Reason, redactedAt, red.RedactedBy,
		},
	})
	if err != nil {
		return fmt.Errorf("registry: record redaction %s/%s@%s: %w",
			red.ProjectID, red.Path, red.VersionID, err)
	}
	return nil
}

func (r *Rqlite) ListRedactions(ctx context.Context, projectID, path string) ([]Redaction, error) {
	stmt := gorqlite.ParameterizedStatement{
		Query: `SELECT ` + redactionColumns + ` FROM redactions
			WHERE project_id = ? ORDER BY redacted_at DESC`,
		Arguments: []interface{}{projectID},
	}
	if path != "" {
		stmt.Query = `SELECT ` + redactionColumns + ` FROM redactions
			WHERE project_id = ? AND path = ? ORDER BY redacted_at DESC`
		stmt.Arguments = []interface{}{projectID, path}
	}

	rows, err := r.conn.QueryOneParameterizedContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("registry: list redactions for %q: %w", projectID, err)
	}
	var out []Redaction
	for rows.Next() {
		m, err := rows.Map()
		if err != nil {
			return nil, fmt.Errorf("registry: scan redaction row: %w", err)
		}
		var red Redaction
		red.ProjectID, _ = m["project_id"].(string)
		red.Path, _ = m["path"].(string)
		red.VersionID, _ = m["version_id"].(string)
		red.Reason, _ = m["reason"].(string)
		red.RedactedAt, _ = m["redacted_at"].(string)
		red.RedactedBy, _ = m["redacted_by"].(string)
		out = append(out, red)
	}
	return out, nil
}
