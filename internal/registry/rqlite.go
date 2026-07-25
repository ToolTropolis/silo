package registry

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rqlite/gorqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ledgerMigration creates schema_migrations itself, so it is applied before the
// ledger can be consulted. Every migration ordered at or before it predates the
// ledger and is backfilled as already-applied on first run.
const ledgerMigration = "003_schema_migrations.sql"

// Rqlite is the default TenantRegistry, backed by a 3-node rqlite cluster
// (SQLite semantics + Raft HA). gorqlite follows leader redirects
// automatically, so pointing it at every node's address lets reads and writes
// survive a leader failover transparently.
type Rqlite struct {
	conn *gorqlite.Connection
}

var _ TenantRegistry = (*Rqlite)(nil)

// NewRqlite connects to the cluster and ensures the schema exists. Pass every
// known node address (e.g. http://localhost:4001, ...:4003, ...:4005) so the
// client can retry against another node when the current one isn't the leader.
func NewRqlite(ctx context.Context, addresses []string) (*Rqlite, error) {
	if len(addresses) == 0 {
		return nil, fmt.Errorf("registry: at least one rqlite address required")
	}
	// gorqlite takes a single connection URL, then auto-discovers the rest of
	// the cluster from /status + /nodes and follows leader failover across the
	// discovered peers. We hand it the first reachable address as the seed; the
	// nodes advertise host-reachable addresses (see deploy/docker-compose.yaml),
	// so discovery yields peers a host client can actually reach.
	conn, err := gorqlite.Open(seedURL(addresses))
	if err != nil {
		return nil, fmt.Errorf("registry: open rqlite: %w", err)
	}
	r := &Rqlite{conn: conn}
	if err := r.ensureSchema(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return r, nil
}

// seedURL returns the seed connection URL gorqlite dials first. It expects a
// single http(s):// URL; the rest of the cluster is discovered from there.
func seedURL(addresses []string) string {
	return addresses[0]
}

// Close releases the connection.
func (r *Rqlite) Close() { r.conn.Close() }

func (r *Rqlite) ensureSchema(ctx context.Context) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("registry: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // apply in filename order (001_, 002_, ...)

	// The ledger has to exist before it can be consulted, so it is applied
	// unconditionally. It is CREATE TABLE IF NOT EXISTS, so that is safe.
	applied, err := r.appliedMigrations(ctx, names)
	if err != nil {
		return err
	}

	for _, name := range names {
		if _, done := applied[name]; done {
			continue
		}
		content, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("registry: read migration %s: %w", name, err)
		}
		// rqlite's WriteOne runs one statement, so split on ';' and skip
		// blanks/comments.
		for _, stmt := range splitStatements(string(content)) {
			if _, err := r.conn.WriteOneContext(ctx, stmt); err != nil {
				return fmt.Errorf("registry: apply %s: %w", name, err)
			}
		}
		if err := r.recordMigration(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// appliedMigrations returns the set of migrations already recorded as applied.
//
// It first applies the ledger migration itself, then backfills every migration
// that predates the ledger. Those are all CREATE TABLE IF NOT EXISTS, so they
// have already run harmlessly against any existing cluster — recording them
// keeps the ledger honest rather than claiming they never ran.
func (r *Rqlite) appliedMigrations(ctx context.Context, all []string) (map[string]struct{}, error) {
	ledger, err := migrationsFS.ReadFile("migrations/" + ledgerMigration)
	if err != nil {
		return nil, fmt.Errorf("registry: read %s: %w", ledgerMigration, err)
	}
	for _, stmt := range splitStatements(string(ledger)) {
		if _, err := r.conn.WriteOneContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("registry: apply %s: %w", ledgerMigration, err)
		}
	}

	rows, err := r.conn.QueryOneContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("registry: read schema_migrations: %w", err)
	}
	applied := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("registry: scan schema_migrations: %w", err)
		}
		applied[name] = struct{}{}
	}

	// First run against a pre-ledger cluster: everything up to and including the
	// ledger has effectively already been applied.
	if len(applied) == 0 {
		for _, name := range all {
			if name > ledgerMigration {
				break // ordered, so anything after the ledger is genuinely new
			}
			if err := r.recordMigration(ctx, name); err != nil {
				return nil, err
			}
			applied[name] = struct{}{}
		}
	}
	return applied, nil
}

func (r *Rqlite) recordMigration(ctx context.Context, name string) error {
	_, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `INSERT OR IGNORE INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
		Arguments: []interface{}{name, time.Now().UTC().Format(time.RFC3339)},
	})
	if err != nil {
		return fmt.Errorf("registry: record migration %s: %w", name, err)
	}
	return nil
}

func (r *Rqlite) Register(ctx context.Context, rec ProjectRecord) error {
	if rec.ProjectID == "" || rec.BucketName == "" {
		return fmt.Errorf("registry: ProjectID and BucketName are required")
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if rec.Status == "" {
		rec.Status = StatusActive
	}
	res, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query: `INSERT INTO projects
			(project_id, bucket_name, credential_id, key_id, created_at, status, generation)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
		Arguments: []interface{}{
			rec.ProjectID, rec.BucketName, rec.CredentialID, rec.KeyID, rec.CreatedAt, rec.Status, rec.Generation,
		},
	})
	if err != nil {
		if isUniqueViolation(err) || isUniqueViolation(res.Err) {
			return fmt.Errorf("registry: project %q already registered: %w", rec.ProjectID, ErrAlreadyExists)
		}
		return fmt.Errorf("registry: register %q: %w", rec.ProjectID, err)
	}
	return nil
}

func (r *Rqlite) Get(ctx context.Context, projectID string) (ProjectRecord, error) {
	rows, err := r.conn.QueryOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query: `SELECT project_id, bucket_name, credential_id, key_id, created_at, status, generation
			FROM projects WHERE project_id = ?`,
		Arguments: []interface{}{projectID},
	})
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("registry: get %q: %w", projectID, err)
	}
	if !rows.Next() {
		return ProjectRecord{}, ErrNotFound
	}
	return scanRecord(rows)
}

func (r *Rqlite) List(ctx context.Context) ([]ProjectRecord, error) {
	rows, err := r.conn.QueryOneContext(ctx,
		`SELECT project_id, bucket_name, credential_id, key_id, created_at, status, generation
			FROM projects ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("registry: list: %w", err)
	}
	var out []ProjectRecord
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func (r *Rqlite) UpdateStatus(ctx context.Context, projectID string, status string) error {
	res, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `UPDATE projects SET status = ? WHERE project_id = ?`,
		Arguments: []interface{}{status, projectID},
	})
	if err != nil {
		return fmt.Errorf("registry: update status %q: %w", projectID, err)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Rqlite) UpdateRefs(ctx context.Context, projectID, keyID, credentialID string) error {
	res, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `UPDATE projects SET key_id = ?, credential_id = ? WHERE project_id = ?`,
		Arguments: []interface{}{keyID, credentialID, projectID},
	})
	if err != nil {
		return fmt.Errorf("registry: update refs %q: %w", projectID, err)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Rqlite) ClearBucket(ctx context.Context, projectID string) error {
	res, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `UPDATE projects SET bucket_name = '' WHERE project_id = ?`,
		Arguments: []interface{}{projectID},
	})
	if err != nil {
		return fmt.Errorf("registry: clear bucket %q: %w", projectID, err)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Rqlite) Deregister(ctx context.Context, projectID string) error {
	res, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `DELETE FROM projects WHERE project_id = ?`,
		Arguments: []interface{}{projectID},
	})
	if err != nil {
		return fmt.Errorf("registry: deregister %q: %w", projectID, err)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// scanRecord reads the standard column order into a ProjectRecord.
func scanRecord(rows gorqlite.QueryResult) (ProjectRecord, error) {
	var rec ProjectRecord
	err := rows.Scan(&rec.ProjectID, &rec.BucketName, &rec.CredentialID, &rec.KeyID,
		&rec.CreatedAt, &rec.Status, &rec.Generation)
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("registry: scan row: %w", err)
	}
	return rec, nil
}

// splitStatements breaks a multi-statement SQL string into individual trimmed
// statements, dropping blanks and full-line comments.
func splitStatements(sql string) []string {
	// Strip -- line comments FIRST (including trailing ones), so a ';' inside a
	// comment can't split a statement, then split on ';'.
	var b strings.Builder
	for _, ln := range strings.Split(sql, "\n") {
		if i := strings.Index(ln, "--"); i >= 0 {
			ln = ln[:i]
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	var out []string
	for _, part := range strings.Split(b.String(), ";") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// isUniqueViolation reports whether err is a SQLite UNIQUE/PK constraint error.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "primary key")
}
