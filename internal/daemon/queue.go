package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
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

	// Bind before draining. A queue belonging to a previous tenant would
	// otherwise be replayed into this project's bucket — a cross-tenant write,
	// worse than a stale read. Binding discards such a queue first.
	//
	// Unverifiable ownership blocks the drain rather than risking that: the
	// writes stay safely queued and the next pass retries once the registry is
	// reachable again.
	if err := d.bindCache(ctx, projectID); err != nil {
		return fmt.Errorf("daemon: sync %q: %w", projectID, err)
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

// ErrQueuedWrites is returned when an operation would discard writes that have
// not reached the durable backend yet.
var ErrQueuedWrites = errors.New("daemon: project has unsynced writes")

// projectPurger is implemented by caches that can drop a project's local store
// entirely. Off LocalCache because it is an operator action rather than part of
// the per-request contract.
type projectPurger interface {
	PurgeProject(ctx context.Context, projectID string) error
}

// PurgeCache removes a project's local cache, refusing while it still holds
// writes that never reached the backend.
//
// The gate lives here rather than only in siloctl because siloctl's check is
// advisory — it prints a note and continues when it cannot reach a daemon. This
// is the enforced one: whatever calls it, buffered writes are not silently
// discarded.
func (d *Daemon) PurgeCache(ctx context.Context, projectID string) error {
	purger, ok := d.cache.(projectPurger)
	if !ok {
		return fmt.Errorf("daemon: purge %q: cache does not support purging", projectID)
	}

	depth, err := d.cache.QueueDepth(ctx, projectID)
	if err != nil {
		// Refuse rather than guess. Purging on an unreadable queue is exactly
		// how buffered writes would vanish unnoticed.
		return fmt.Errorf("daemon: purge %q: cannot read queue depth: %w", projectID, err)
	}
	if depth > 0 {
		return fmt.Errorf("daemon: purge %q: %d write(s) still queued: %w", projectID, depth, ErrQueuedWrites)
	}
	return purger.PurgeProject(ctx, projectID)
}

// cacheEvictor is implemented by caches that can bound their own size. Off
// LocalCache for the same reason as the purger: an operator concern rather than
// part of the per-request contract.
type cacheEvictor interface {
	Evict(ctx context.Context, projectID string, policy cache.EvictPolicy) (cache.EvictResult, error)
	Stats(ctx context.Context, projectID string) (cache.CacheStats, error)
}

// EvictCache applies a retention policy to a project's cached content.
//
// The queue is never touched — those are writes that never reached the backend,
// so removing them would be data loss rather than cache management.
func (d *Daemon) EvictCache(ctx context.Context, projectID string, policy cache.EvictPolicy) (cache.EvictResult, error) {
	evictor, ok := d.cache.(cacheEvictor)
	if !ok {
		return cache.EvictResult{}, fmt.Errorf("daemon: evict %q: cache does not support eviction", projectID)
	}
	return evictor.Evict(ctx, projectID, policy)
}

// CacheStats reports a project's cache size, both as live content and as the
// file on disk. The two diverge because bbolt never shrinks a file, which is
// what makes compaction worth doing.
func (d *Daemon) CacheStats(ctx context.Context, projectID string) (cache.CacheStats, error) {
	evictor, ok := d.cache.(cacheEvictor)
	if !ok {
		return cache.CacheStats{}, fmt.Errorf("daemon: stats %q: cache does not support stats", projectID)
	}
	return evictor.Stats(ctx, projectID)
}

// cacheCompactor is implemented by caches that can reclaim their own disk.
type cacheCompactor interface {
	Compact(ctx context.Context, projectID string) (cache.CompactResult, error)
}

// CompactCache rewrites a project's cache file to return the disk that eviction
// freed but bbolt kept. Refuses while writes are queued — see cache.Compact.
func (d *Daemon) CompactCache(ctx context.Context, projectID string) (cache.CompactResult, error) {
	compactor, ok := d.cache.(cacheCompactor)
	if !ok {
		return cache.CompactResult{}, fmt.Errorf("daemon: compact %q: cache does not support compaction", projectID)
	}
	return compactor.Compact(ctx, projectID)
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
