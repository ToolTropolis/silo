package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/rqlite/gorqlite"
)

// NewGeneration mints an opaque identifier for one incarnation of a project.
//
// Random rather than derived: a timestamp collides on a fast re-onboard and can
// be reproduced by a restore, and the credential and key refs are cleared by
// teardown before the record is deleted (and rotating a key must not invalidate
// a live cache). 128 bits of randomness makes collision not worth reasoning
// about.
func NewGeneration() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("registry: mint generation: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// backfillGenerations gives a generation to every project registered before
// 004 added the column.
//
// Those records carry an empty generation, which the daemon treats as
// unverifiable — so it refuses to serve the read path's outage fallback and the
// project can never use its cache. Filling them is what makes an existing
// deployment cache again after upgrading.
//
// Deliberately not done in SQL and deliberately not done in the daemon:
//
//   - In SQL, rqlite rewrites non-deterministic functions on the leader before
//     replication, so randomblob() yields ONE value for the whole statement and
//     every backfilled project would share a generation — one project's cache
//     file would satisfy another's bind. See migrations/008.
//   - In the daemon, minting at first read would stamp a fresh generation onto
//     whatever cache file already sits on disk, adopting a previous tenant's
//     content as the new project's own. That is precisely the hole 004 closed.
//
// Minting here, before any read, means the new generation cannot match the file
// already on disk, so BindProject discards its content as foreign. Queued
// writes survive that path: they are unsynced data, not another tenant's bytes.
//
// Best-effort per project: one failure must not stop a daemon from starting,
// since the only consequence of a still-empty generation is a cold cache.
func (r *Rqlite) backfillGenerations(ctx context.Context) error {
	rows, err := r.conn.QueryOneContext(ctx,
		`SELECT project_id FROM projects WHERE generation IS NULL OR generation = ''`)
	if err != nil {
		return fmt.Errorf("registry: find projects without a generation: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("registry: scan project_id: %w", err)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}

	for _, id := range ids {
		gen, err := NewGeneration()
		if err != nil {
			return err
		}
		// Guarded on the empty generation so a concurrent daemon doing the same
		// backfill cannot overwrite a generation the other just minted — the
		// loser's UPDATE matches no rows instead of rewriting a live value.
		if _, err := r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
			Query: `UPDATE projects SET generation = ?
			         WHERE project_id = ? AND (generation IS NULL OR generation = '')`,
			Arguments: []any{gen, id},
		}); err != nil {
			return fmt.Errorf("registry: backfill generation for %q: %w", id, err)
		}
	}
	return nil
}
