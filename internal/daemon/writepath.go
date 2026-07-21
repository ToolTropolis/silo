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

// SafeWrite is the CAS write path: read the current version, apply edit, then
// Put with an If-Match on the current ETag. On a precondition failure someone
// else wrote in the meantime, so it retries. When the backend is unreachable it
// falls back to the local cache queue, drained by the sync worker on recovery.
//
// This mirrors the pseudocode in the spec (Section 3.6). The concurrent-write
// conflict test in the v1 definition of done exercises the retry branch.
func (d *Daemon) SafeWrite(ctx context.Context, projectID, path string, edit func([]byte) []byte, actor, sessionID string) error {
	for attempt := 0; attempt < maxRetries; attempt++ {
		current, ver, err := d.backend.Get(ctx, projectID, path, "")
		if err != nil && !errors.Is(err, backend.ErrNotFound) {
			// Backend unreachable: fall back to local cache + queue.
			return d.cache.Enqueue(ctx, projectID, cache.PendingWrite{
				Path:      path,
				Content:   edit(nil),
				Actor:     actor,
				SessionID: sessionID,
			})
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
			return err
		}

		_ = d.cache.Put(ctx, projectID, path, newContent) // keep local cache warm
		return nil
	}
	return ErrTooManyConflicts
}
