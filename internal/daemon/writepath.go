package daemon

import (
	"context"
	"errors"

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
	for attempt := 0; attempt < maxRetries; attempt++ {
		current, ver, err := d.backend.Get(ctx, projectID, path, "")
		if err != nil && !errors.Is(err, backend.ErrNotFound) {
			// Backend unreachable: fall back to local cache + queue.
			newContent := edit(nil)
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

		newContent := edit(current)

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

		_ = d.cache.Put(ctx, projectID, path, newContent) // keep local cache warm
		return WriteDurable, nil
	}
	return WriteDurable, ErrTooManyConflicts
}
