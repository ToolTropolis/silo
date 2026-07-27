// Package client is the agent-facing access layer between an agent (or the
// harness around it) and Silo's local cache / durable backend. Agents call
// Read/Write/List/Search on markdown content — the same text they'd write to a
// plain .md file — and never talk to bbolt or SeaweedFS directly.
//
// The SDK is initialized with a ProjectID and a daemon endpoint (a local Unix
// socket for same-machine agents, or a network address) and authenticates with
// the project-scoped credential issued at onboarding; it never sees another
// project's credential or key.
package client

import (
	"context"
	"errors"
)

// Client is what an agent (or the harness around it) imports directly.
// Every method operates on markdown content — callers read/write the
// same text they'd write to a plain .md file; the SDK handles routing
// to the local cache and, transparently, to the durable backend via
// the daemon's write path (hash/ETag CAS, versioning, etc.)
type Client interface {
	// Read returns the current markdown content at path within the
	// caller's project scope.
	Read(ctx context.Context, path string) ([]byte, error)

	// Write persists new markdown content at path. Internally goes
	// through the daemon's SafeWrite (CAS + versioning) — the caller
	// never needs to think about conflicts or retries.
	Write(ctx context.Context, path string, content []byte) error
	// WriteAs records who wrote it, so an operator can attribute a memory to
	// the agent that produced it. An empty actor behaves exactly like Write.
	WriteAs(ctx context.Context, path string, content []byte, actor string) error

	// List returns memory paths under a prefix (mirrors browsing a
	// directory of .md files).
	List(ctx context.Context, pathPrefix string) ([]string, error)

	// Search does a simple substring/grep-style search across memory
	// content under a prefix.
	Search(ctx context.Context, pathPrefix, query string) ([]SearchResult, error)
}

// SearchResult is one match from Search.
type SearchResult struct {
	Path    string `json:"Path"`
	Snippet string `json:"Snippet"`
}

// ErrNotFound is returned by Read when no content exists at the path.
var ErrNotFound = errors.New("client: not found")

// ErrUnauthorized is returned when the token is missing, unknown, or not scoped
// to the requested project.
var ErrUnauthorized = errors.New("client: unauthorized")

// ErrTooLarge is returned by Write when the content exceeds the project's
// per-entry size cap.
//
// Distinct from a transport failure: the write was understood and refused, so
// retrying the same content will never succeed. Split the content into smaller
// files instead.
var ErrTooLarge = errors.New("client: entry exceeds the size limit")

// ErrReadOnly is returned by Write when the token authenticates but grants only
// read access.
//
// Distinct from ErrUnauthorized on purpose: the credential is valid, so a caller
// that retries or re-authenticates is wasting its time. This is the error that
// tells an agent the memory it is holding is reference material it may not
// change.
var ErrReadOnly = errors.New("client: token is read-only")
