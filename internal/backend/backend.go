// Package backend defines the durable storage abstraction and its adapters.
//
// The SeaweedFS adapter (seaweedfs.go) is the default implementation; any
// S3-compatible backend can be swapped in without touching callers.
package backend

import (
	"context"
	"errors"
)

// ObjectVersion identifies a specific version of a stored memory object.
type ObjectVersion struct {
	VersionID  string
	ETag       string
	ModifiedAt string // RFC3339
}

// PutOptions carries the metadata attached to a write (actor, session, etc.)
// and the optional CAS precondition.
type PutOptions struct {
	IfMatchETag string // empty = unconditional write
	Actor       string // agent ID or human user ID
	SessionID   string
	Tags        map[string]string
}

// DurableBackend is the interface every storage adapter implements.
// The SeaweedFS adapter is the default (seaweedfs.go); swap in another
// S3-compatible implementation without touching callers.
type DurableBackend interface {
	// Put writes content to path within a project's bucket. Returns the
	// resulting version. If opts.IfMatchETag is set and doesn't match the
	// current object's ETag, returns ErrPreconditionFailed.
	Put(ctx context.Context, projectID, path string, content []byte, opts PutOptions) (ObjectVersion, error)

	// Get fetches the current (or a specific) version of an object.
	Get(ctx context.Context, projectID, path string, versionID string) ([]byte, ObjectVersion, error)

	// ListVersions returns version history for a path, newest first.
	ListVersions(ctx context.Context, projectID, path string) ([]ObjectVersion, error)

	// Delete removes an object (creates a delete marker; version history persists).
	Delete(ctx context.Context, projectID, path string) error

	// CreateBucket provisions a new project's bucket with versioning enabled.
	// Called only from the onboarding path.
	CreateBucket(ctx context.Context, projectID string) error

	// DeleteBucket is the destructive teardown step. Called only from the
	// manual teardown flow, one explicit call per layer.
	DeleteBucket(ctx context.Context, projectID string) error
}

// ErrPreconditionFailed is returned by Put when IfMatchETag doesn't match the
// current object — i.e. a concurrent write landed first.
var ErrPreconditionFailed = errors.New("backend: precondition failed (concurrent write)")

// ErrNotFound is returned by Get when no object exists at the path.
var ErrNotFound = errors.New("backend: object not found")

// errNotImplemented is returned by adapter stubs whose real logic isn't built
// yet. Kept unexported so callers only ever see it as an opaque error.
var errNotImplemented = errors.New("backend: not implemented")
