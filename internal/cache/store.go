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

	// BindProject verifies that a project's local store belongs to the given
	// generation, discarding anything that belonged to a previous one.
	//
	// On the LocalCache interface rather than behind a type assertion — unlike
	// the reporting-only additions — because it is a tenant boundary, not a
	// nicety. A cache implementation that silently skipped the check would be
	// an isolation hole with no compile error to catch it.
	//
	// Callers bind once per project before first use; the check is expected to
	// be cheap enough to be idempotent but is not on the read path.
	BindProject(ctx context.Context, projectID, generation string) error

	// QueueDepth reports how many writes are buffered for a project without
	// consuming them.
	//
	// Divergence from the spec's §3.2 interface, added deliberately: DrainQueue
	// is destructive, so with the original five methods the amount of unsynced
	// data was unobservable — counting the backlog meant emptying it. There is
	// no way to express "how much is at risk on this disk?" otherwise, and that
	// question has to be answerable before a shutdown or a teardown.
	QueueDepth(ctx context.Context, projectID string) (int, error)
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

// ErrCorruptEntry is returned when a stored entry does not carry a valid
// header. Callers treat it as a miss: the backend is the source of truth, so a
// damaged cache byte must not become an outage.
var ErrCorruptEntry = errors.New("cache: corrupt entry")

// ErrCacheLocked is returned when another process holds the bbolt lock on a
// project's cache file. Two Silo processes sharing a cache directory is a
// configuration mistake, not a transient condition, so it is named rather than
// surfaced as an opaque timeout.
var ErrCacheLocked = errors.New("cache: file is locked by another process")

// ErrNoGeneration is returned by BindProject when the caller has no generation
// to check against. It is deliberately an error rather than a permissive
// default: binding with an empty generation would make every file match, which
// is exactly the hole the check closes.
var ErrNoGeneration = errors.New("cache: no generation supplied")
