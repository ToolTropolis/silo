package registry

import (
	"context"
	"fmt"
	"time"

	"github.com/rqlite/gorqlite"
)

// Locker is per-project leader election. Exactly one daemon instance can hold a
// project's lock at a time; the write path is gated on holding it. Backed by
// rqlite (Raft), so acquisition is linearizable across daemon instances.
type Locker interface {
	// Acquire tries to take (or renew) the lock for a project on behalf of
	// owner, for the given lease duration. Returns true if the caller now holds
	// it. A lock whose lease has expired can be taken by anyone.
	Acquire(ctx context.Context, projectID, owner string, lease time.Duration) (bool, error)
	// Release drops the lock if owner holds it. Releasing a lock you don't hold
	// is a no-op.
	Release(ctx context.Context, projectID, owner string) error
}

var _ Locker = (*Rqlite)(nil)

// Acquire atomically inserts the lock if absent, or takes it over if the
// existing lease has expired, or renews it if the caller already owns it. The
// single INSERT ... ON CONFLICT statement is linearized by rqlite's Raft log,
// so two daemons racing to acquire cannot both win.
func (r *Rqlite) Acquire(ctx context.Context, projectID, owner string, lease time.Duration) (bool, error) {
	if projectID == "" || owner == "" {
		return false, fmt.Errorf("registry: Acquire requires projectID and owner")
	}
	now := time.Now().Unix()
	expires := time.Now().Add(lease).Unix()

	// ON CONFLICT fires when the row exists. We only overwrite when the current
	// owner is us (renew) OR the existing lease has expired (takeover); the
	// WHERE clause enforces that, so a live lock held by someone else is left
	// untouched.
	res, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query: `INSERT INTO project_locks (project_id, owner, expires_at)
			VALUES (?, ?, ?)
			ON CONFLICT(project_id) DO UPDATE SET
				owner = excluded.owner,
				expires_at = excluded.expires_at
			WHERE project_locks.owner = excluded.owner
			   OR project_locks.expires_at < ?`,
		Arguments: []interface{}{projectID, owner, expires, now},
	})
	if err != nil {
		return false, fmt.Errorf("registry: acquire lock %q: %w", projectID, err)
	}
	// RowsAffected > 0 means the insert or the conditional update applied, i.e.
	// we now hold the lock. 0 means someone else holds a live lease.
	return res.RowsAffected > 0, nil
}

// Release drops the lock only if owner currently holds it.
func (r *Rqlite) Release(ctx context.Context, projectID, owner string) error {
	_, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `DELETE FROM project_locks WHERE project_id = ? AND owner = ?`,
		Arguments: []interface{}{projectID, owner},
	})
	if err != nil {
		return fmt.Errorf("registry: release lock %q: %w", projectID, err)
	}
	return nil
}
