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

	// ListPaths returns the object keys under a prefix (current versions only,
	// delete markers excluded).
	//
	// ADDITIVE CONTRACT EXTENSION beyond the spec's §3.1 interface: the spec's
	// pkg/client (§3.7) defines List(pathPrefix) and Search(pathPrefix, query),
	// and neither is implementable without path enumeration — ListVersions only
	// covers versions of an already-known path. S3/SeaweedFS support this
	// natively (ListObjectsV2), so this is a thin adapter addition. Additive
	// only: no existing signature changed.
	ListPaths(ctx context.Context, projectID, prefix string) ([]string, error)

	// Delete removes an object (creates a delete marker; version history persists).
	Delete(ctx context.Context, projectID, path string) error

	// DeleteVersion destroys ONE version's bytes, leaving every other version of
	// the path intact. Unlike Delete it creates no delete marker and is not
	// reversible.
	//
	// This is the redaction primitive: a credential written into memory is
	// otherwise in that version forever, and the only remedy was destroying the
	// whole project. The destruction is real by design — a tombstone the object
	// store still holds is not erasure — so the audit of what was removed is
	// recorded in the registry, which the delete cannot reach.
	//
	// Callers must refuse to redact the current version: removing the head would
	// silently revert the path to older content. Enforced above this layer,
	// where the head version is known.
	DeleteVersion(ctx context.Context, projectID, path, versionID string) error

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

// ErrNoVersionID is returned by DeleteVersion when no version was named.
// Deleting without one is DeleteObject, which hides the entire path behind a
// delete marker rather than removing a single version — a far worse outcome
// than the caller asked for, so it is refused rather than guessed at.
var ErrNoVersionID = errors.New("backend: no version ID supplied")

// errNotImplemented is returned by adapter stubs whose real logic isn't built
// yet. Kept unexported so callers only ever see it as an opaque error.
var errNotImplemented = errors.New("backend: not implemented")
