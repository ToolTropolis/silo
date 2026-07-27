package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
)

// maxRetries bounds the CAS retry loop before SafeWrite gives up.
const maxRetries = 5

// ErrTooManyConflicts is returned when SafeWrite exhausts its retries losing the
// ETag CAS race every time.
var ErrTooManyConflicts = errors.New("daemon: too many concurrent write conflicts")

// WriteOutcome reports where a write actually landed.
//
// This exists because "your memory is safely versioned in the backend" and "your
// memory is on one machine's local disk" are very different guarantees, and
// SafeWrite previously returned a bare nil for both.
type WriteOutcome int

const (
	// WriteDurable means the content reached the versioned backend.
	WriteDurable WriteOutcome = iota
	// WriteQueued means the backend was unreachable and the write is buffered
	// locally, awaiting replay by the sync worker. It is NOT yet safe from the
	// loss of this host's disk.
	WriteQueued
)

func (o WriteOutcome) String() string {
	if o == WriteQueued {
		return "queued"
	}
	return "durable"
}

// SafeWrite is the CAS write path: read the current version, apply edit, then
// Put with an If-Match on the current ETag. On a precondition failure someone
// else wrote in the meantime, so it retries. When the backend is unreachable it
// falls back to the local cache queue, drained by the sync worker on recovery.
//
// The returned WriteOutcome distinguishes those two endings. Callers that treat
// a queued write as durable will report success for data that exists only on
// this host — which is exactly what the HTTP layer used to do.
//
// This mirrors the pseudocode in the spec (Section 3.6). The concurrent-write
// conflict test in the v1 definition of done exercises the retry branch.
func (d *Daemon) SafeWrite(ctx context.Context, projectID, path string, edit func([]byte) []byte, actor, sessionID string) (WriteOutcome, error) {
	return d.safeWrite(ctx, projectID, path, edit, actor, sessionID, "")
}

// SafeWriteIfMatch is SafeWrite with an optimistic-concurrency precondition:
// the write applies only if the stored content still hashes to expectedHash,
// and returns ErrPreconditionMismatch otherwise.
//
// Separate from SafeWrite rather than a sixth parameter on it. SafeWrite has
// five callers — the Distilator's writer interface, the sync worker's queue
// drain, the HTTP layer — and only one of them has a precondition to express;
// widening the shared signature would make every other call site carry an empty
// string forever.
//
// An empty expectedHash is exactly SafeWrite. Use ContentHash("") to require
// that nothing exists at the path yet.
func (d *Daemon) SafeWriteIfMatch(ctx context.Context, projectID, path string,
	edit func([]byte) []byte, actor, sessionID, expectedHash string) (WriteOutcome, error) {
	return d.safeWrite(ctx, projectID, path, edit, actor, sessionID, expectedHash)
}

func (d *Daemon) safeWrite(ctx context.Context, projectID, path string, edit func([]byte) []byte,
	actor, sessionID, expectedHash string) (WriteOutcome, error) {
	for attempt := 0; attempt < maxRetries; attempt++ {
		current, ver, err := d.backend.Get(ctx, projectID, path, "")
		if err != nil && !errors.Is(err, backend.ErrNotFound) {
			// Backend unreachable: fall back to local cache + queue.
			//
			// Bind first. Queuing into a file that belongs to a previous tenant
			// would mix this project's writes with theirs, and the sync worker
			// would then replay both into this project's bucket. Binding
			// discards the stale contents before anything is added.
			//
			// A bind failure is not fatal here: the write still has to go
			// somewhere, and an unverified queue is safe as long as it is never
			// read back cross-tenant, which the read path enforces separately.
			if bindErr := d.bindCache(ctx, projectID); bindErr != nil && !errors.Is(bindErr, ErrCacheUnverified) {
				return WriteDurable, bindErr
			}
			// A precondition cannot be honoured against an unreachable backend:
			// the local cache may be stale, so a hash that matches it proves
			// nothing about what is actually stored. Refusing is the only
			// honest answer — silently dropping the precondition would let a
			// caller believe a conflict check happened when none did.
			if expectedHash != "" {
				return WriteDurable, fmt.Errorf(
					"%w: the backend is unreachable, so the stored content cannot be "+
						"checked; retry when it recovers or write without a precondition",
					ErrPreconditionMismatch)
			}
			newContent := edit(nil)
			// Checked on the queued path too. If the cap only applied when the
			// backend was up, an outage would become the way around it — and
			// the oversized entry would sit on local disk until the backend
			// returned, then be replayed into the bucket anyway.
			if limitErr := d.checkEntrySize(projectID, path, len(newContent)); limitErr != nil {
				return WriteDurable, limitErr
			}
			if qErr := d.cache.Enqueue(ctx, projectID, cache.PendingWrite{
				Path:      path,
				Content:   newContent,
				Actor:     actor,
				SessionID: sessionID,
			}); qErr != nil {
				return WriteDurable, qErr
			}
			// Cache the content too, not just the queue entry. Without this an
			// agent that writes during an outage cannot read back what it just
			// wrote, since Read falls back to the cache and would miss it.
			_ = d.cache.Put(ctx, projectID, path, newContent)
			return WriteQueued, nil
		}

		// Checked against the content just fetched, before edit runs and before
		// anything is written — so a rejected precondition creates no version.
		// Inside the retry loop because a CAS retry re-reads: if another writer
		// landed in the gap, the caller's hash is genuinely stale and must be
		// reported rather than silently retried against the new content.
		if preErr := checkPrecondition(expectedHash, current); preErr != nil {
			return WriteDurable, preErr
		}

		newContent := edit(current)
		// Before the Put, so an oversized write never reaches the backend and
		// never creates a version. Inside the retry loop rather than above it
		// because edit() is what produces the content: on a CAS retry it runs
		// again against fresher content and can produce a different size.
		if limitErr := d.checkEntrySize(projectID, path, len(newContent)); limitErr != nil {
			return WriteDurable, limitErr
		}

		_, err = d.backend.Put(ctx, projectID, path, newContent, backend.PutOptions{
			IfMatchETag: ver.ETag,
			Actor:       actor,
			SessionID:   sessionID,
		})
		if errors.Is(err, backend.ErrPreconditionFailed) {
			continue // someone else wrote in the meantime — retry
		}
		if err != nil {
			return WriteDurable, err
		}

		// Bind before warming, so the entry lands in a file stamped as this
		// project's. Warming an unstamped file would just have it discarded by
		// the first bind that ran later.
		if bindErr := d.bindCache(ctx, projectID); bindErr == nil {
			_ = d.cache.Put(ctx, projectID, path, newContent) // keep local cache warm
		}
		return WriteDurable, nil
	}
	return WriteDurable, ErrTooManyConflicts
}
