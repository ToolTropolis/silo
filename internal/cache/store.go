// Package cache is the bbolt-backed local fast path agents read/write during a
// live session. One bbolt instance per project. It never talks to the network
// directly — the daemon's write path syncs to the durable backend.
package cache

import (
	"context"
	"errors"
)

// LocalCache is the bbolt-backed fast path agents actually read/write
// during a live session. It never talks to the network directly — the
// daemon's write path is responsible for syncing to DurableBackend.
type LocalCache interface {
	Get(ctx context.Context, projectID, path string) ([]byte, error)
	Put(ctx context.Context, projectID, path string, content []byte) error
	Delete(ctx context.Context, projectID, path string) error

	// Enqueue records a write that couldn't sync to the durable backend
	// (backend unreachable). Drained by the sync worker on recovery.
	Enqueue(ctx context.Context, projectID string, w PendingWrite) error
	DrainQueue(ctx context.Context, projectID string) ([]PendingWrite, error)
}

// PendingWrite is a write buffered locally while the durable backend was
// unreachable, replayed by the sync worker once it recovers.
type PendingWrite struct {
	Path      string
	Content   []byte
	Actor     string
	SessionID string
	QueuedAt  string
}

// ErrNotFound is returned by Get when no entry exists at the path.
var ErrNotFound = errors.New("cache: entry not found")
