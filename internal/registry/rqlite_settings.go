package registry

import (
	"context"
	"fmt"
	"time"

	"github.com/rqlite/gorqlite"
)

var _ SettingsStore = (*Rqlite)(nil)

// settingsColumns is the standard select order, shared by Get and List so the
// two cannot drift.
const settingsColumns = `project_id, cache_ttl_secs, cache_max_entries, cache_max_bytes, ` +
	`max_entry_bytes, updated_at, updated_by`

func (r *Rqlite) GetSettings(ctx context.Context, projectID string) (CacheSettings, error) {
	rows, err := r.conn.QueryOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `SELECT ` + settingsColumns + ` FROM project_settings WHERE project_id = ?`,
		Arguments: []interface{}{projectID},
	})
	if err != nil {
		return CacheSettings{}, fmt.Errorf("registry: get settings %q: %w", projectID, err)
	}
	if !rows.Next() {
		// No row means "inherit", not "missing". Returning ErrNotFound here would
		// force every caller to special-case the common path.
		return CacheSettings{}, nil
	}
	_, s, err := scanSettings(rows)
	return s, err
}

func (r *Rqlite) ListSettings(ctx context.Context) (map[string]CacheSettings, error) {
	rows, err := r.conn.QueryOneContext(ctx, `SELECT `+settingsColumns+` FROM project_settings`)
	if err != nil {
		return nil, fmt.Errorf("registry: list settings: %w", err)
	}
	out := map[string]CacheSettings{}
	for rows.Next() {
		id, s, err := scanSettings(rows)
		if err != nil {
			return nil, err
		}
		out[id] = s
	}
	return out, nil
}

func (r *Rqlite) PutSettings(ctx context.Context, projectID string, s CacheSettings) error {
	if projectID == "" {
		return fmt.Errorf("registry: put settings: project ID required")
	}
	updatedAt := s.UpdatedAt
	if updatedAt == "" {
		updatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	// Nil fields are bound as SQL NULL, which is what restores inheritance for
	// that field. Binding 0 instead would silently pin the project to "never
	// cache" — the exact confusion the nullable columns exist to prevent.
	var ttl, maxEntries, maxBytes, maxEntryBytes interface{}
	if s.TTL != nil {
		ttl = int64(*s.TTL / time.Second)
	}
	if s.MaxEntries != nil {
		maxEntries = int64(*s.MaxEntries)
	}
	if s.MaxBytes != nil {
		maxBytes = *s.MaxBytes
	}
	if s.MaxEntryBytes != nil {
		maxEntryBytes = *s.MaxEntryBytes
	}

	_, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query: `INSERT INTO project_settings
			(project_id, cache_ttl_secs, cache_max_entries, cache_max_bytes,
			 max_entry_bytes, updated_at, updated_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_id) DO UPDATE SET
				cache_ttl_secs = excluded.cache_ttl_secs,
				cache_max_entries = excluded.cache_max_entries,
				cache_max_bytes = excluded.cache_max_bytes,
				max_entry_bytes = excluded.max_entry_bytes,
				updated_at = excluded.updated_at,
				updated_by = excluded.updated_by`,
		Arguments: []interface{}{projectID, ttl, maxEntries, maxBytes, maxEntryBytes,
			updatedAt, s.UpdatedBy},
	})
	if err != nil {
		return fmt.Errorf("registry: put settings %q: %w", projectID, err)
	}
	return nil
}

func (r *Rqlite) DeleteSettings(ctx context.Context, projectID string) error {
	// No RowsAffected check: deleting settings that were never set is the
	// caller getting what they asked for, not an error.
	if _, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `DELETE FROM project_settings WHERE project_id = ?`,
		Arguments: []interface{}{projectID},
	}); err != nil {
		return fmt.Errorf("registry: delete settings %q: %w", projectID, err)
	}
	return nil
}

// scanSettings reads one row via Map, which reports a NULL column as a nil
// value — the distinction the whole schema is built around. Scan into typed
// destinations would coerce NULL to the zero value and erase it.
func scanSettings(rows gorqlite.QueryResult) (string, CacheSettings, error) {
	m, err := rows.Map()
	if err != nil {
		return "", CacheSettings{}, fmt.Errorf("registry: scan settings row: %w", err)
	}

	var s CacheSettings
	projectID, _ := m["project_id"].(string)
	s.UpdatedAt, _ = m["updated_at"].(string)
	s.UpdatedBy, _ = m["updated_by"].(string)

	if secs, ok := asInt64(m["cache_ttl_secs"]); ok {
		d := time.Duration(secs) * time.Second
		s.TTL = &d
	}
	if n, ok := asInt64(m["cache_max_entries"]); ok {
		v := int(n)
		s.MaxEntries = &v
	}
	if n, ok := asInt64(m["cache_max_bytes"]); ok {
		v := n
		s.MaxBytes = &v
	}
	if n, ok := asInt64(m["max_entry_bytes"]); ok {
		v := n
		s.MaxEntryBytes = &v
	}
	return projectID, s, nil
}

// asInt64 normalizes the numeric shapes a JSON-decoded rqlite row can carry.
// A nil value is a SQL NULL and reports false, which is what keeps "unset"
// distinct from zero.
func asInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case nil:
		return 0, false
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}
