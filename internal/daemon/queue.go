package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/tooltropolis/silo/internal/backend"
)

// SyncProject drains a project's local write queue back to the durable backend,
// replaying each buffered write through SafeWrite so it gets the same
// CAS/versioning treatment as a normal write. Called when the backend recovers.
//
// It only drains once it confirms the backend is reachable, so a still-down
// backend doesn't consume the queue and lose the writes. Each replayed write is
// a full-content overwrite (that's what was buffered), applied last-writer-wins
// within the queue's FIFO order.
//
// On a replay failure it re-enqueues the remaining writes (including the one
// that failed) so nothing is dropped, and returns the error — the caller can
// retry the whole drain later.
func (d *Daemon) SyncProject(ctx context.Context, projectID string) error {
	if err := d.backendReachable(ctx, projectID); err != nil {
		return fmt.Errorf("daemon: sync %q: backend not reachable: %w", projectID, err)
	}

	pending, err := d.cache.DrainQueue(ctx, projectID)
	if err != nil {
		return fmt.Errorf("daemon: sync %q: drain queue: %w", projectID, err)
	}

	for i, w := range pending {
		content := w.Content // capture for the closure
		outcome, err := d.SafeWrite(ctx, projectID, w.Path, func([]byte) []byte { return content }, w.Actor, w.SessionID)
		// A replay that queues instead of landing means the backend died mid-drain.
		// SafeWrite has already put that write back on the queue, so treat it as a
		// failure and stop — re-enqueueing below would buffer it a second time.
		if err == nil && outcome == WriteQueued {
			for _, rem := range pending[i+1:] {
				if reErr := d.cache.Enqueue(ctx, projectID, rem); reErr != nil {
					return fmt.Errorf("daemon: sync %q: backend went away mid-drain and re-enqueue failed: %w", projectID, reErr)
				}
			}
			return fmt.Errorf("daemon: sync %q: backend went away while replaying %q", projectID, w.Path)
		}
		if err != nil {
			// Re-enqueue this write and every write after it, preserving order,
			// so a mid-drain failure never drops buffered writes.
			for _, rem := range pending[i:] {
				if reErr := d.cache.Enqueue(ctx, projectID, rem); reErr != nil {
					return fmt.Errorf("daemon: sync %q: replay failed (%v) and re-enqueue failed: %w", projectID, err, reErr)
				}
			}
			return fmt.Errorf("daemon: sync %q: replay %q: %w", projectID, w.Path, err)
		}
	}
	return nil
}

// QueueDepth reports how many of a project's writes are still buffered locally,
// i.e. accepted from an agent but not yet durable. Non-destructive, so it is
// safe to poll from the sync worker and from operator surfaces.
func (d *Daemon) QueueDepth(ctx context.Context, projectID string) (int, error) {
	return d.cache.QueueDepth(ctx, projectID)
}

// oldestQueuer is implemented by caches that can report the head of the queue.
// Kept off LocalCache: the timestamp is a reporting nicety, and the interface is
// the contract every implementation must satisfy.
type oldestQueuer interface {
	OldestQueued(ctx context.Context, projectID string) (string, error)
}

// OldestQueued returns when the oldest still-unsynced write was buffered, or ""
// if the cache can't report it. Depth alone is hard to act on: "12 pending"
// reads very differently from "12 pending, oldest 3 hours ago".
func (d *Daemon) OldestQueued(ctx context.Context, projectID string) (string, error) {
	q, ok := d.cache.(oldestQueuer)
	if !ok {
		return "", nil
	}
	return q.OldestQueued(ctx, projectID)
}

// backendReachable probes the backend with a harmless Get so a drain doesn't
// start against a still-down backend. A not-found result means the backend is
// up (it answered); any other error means it's still unreachable.
func (d *Daemon) backendReachable(ctx context.Context, projectID string) error {
	_, _, err := d.backend.Get(ctx, projectID, ".silo-health", "")
	if err != nil && !errors.Is(err, backend.ErrNotFound) {
		return err
	}
	return nil
}
