package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
)

// ErrNotFound is returned when no content exists at a path.
var ErrNotFound = errors.New("daemon: not found")

// Read returns the current content at path. It serves from the durable backend
// (the source of truth) and warms the local cache; when the backend is
// unreachable it falls back to the cache so a live session keeps working
// offline.
func (d *Daemon) Read(ctx context.Context, projectID, path string) ([]byte, error) {
	content, _, err := d.backend.Get(ctx, projectID, path, "")
	switch {
	case err == nil:
		_ = d.cache.Put(ctx, projectID, path, content) // keep the cache warm
		return content, nil
	case errors.Is(err, backend.ErrNotFound):
		// The backend is the source of truth, so a 404 is a positive statement
		// that this path does not exist. A cached entry for it is known-wrong
		// and must not survive to be served by the fallback below during a
		// later outage. Best-effort, like the cache-warm above.
		//
		// Safe because the adapter classifies not-found strictly (a typed
		// NoSuchKey or an actual 404, never a 5xx or a reset), so an unreachable
		// backend can't reach this branch and drop good entries.
		_ = d.cache.Delete(ctx, projectID, path)
		return nil, ErrNotFound
	}

	// Backend unreachable — fall back to the local cache.
	cached, cacheErr := d.cache.Get(ctx, projectID, path)
	if cacheErr == nil {
		return cached, nil
	}
	if errors.Is(cacheErr, cache.ErrNotFound) {
		return nil, ErrNotFound
	}
	return nil, fmt.Errorf("daemon: read %q: backend unreachable (%v) and cache miss: %w", path, err, cacheErr)
}

// List returns memory paths under a prefix, newest-agnostic and sorted by the
// backend's own ordering. Mirrors browsing a directory of .md files.
func (d *Daemon) List(ctx context.Context, projectID, prefix string) ([]string, error) {
	paths, err := d.backend.ListPaths(ctx, projectID, prefix)
	if err != nil {
		return nil, fmt.Errorf("daemon: list %q: %w", prefix, err)
	}
	return paths, nil
}

// Search does a substring scan across memory content under a prefix — the
// SDK's answer to "let agents grep their memory" without a mounted filesystem.
// Case-insensitive; returns one result per matching path with a short snippet
// around the first match.
func (d *Daemon) Search(ctx context.Context, projectID, prefix, query string) ([]SearchHit, error) {
	if query == "" {
		return nil, fmt.Errorf("daemon: search requires a query")
	}
	paths, err := d.List(ctx, projectID, prefix)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(query)

	var hits []SearchHit
	for _, p := range paths {
		content, err := d.Read(ctx, projectID, p)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // raced with a delete; skip
			}
			return nil, err
		}
		idx := strings.Index(strings.ToLower(string(content)), needle)
		if idx < 0 {
			continue
		}
		hits = append(hits, SearchHit{Path: p, Snippet: snippet(string(content), idx, len(query))})
	}
	return hits, nil
}

// SearchHit is one match from Search.
type SearchHit struct {
	Path    string
	Snippet string
}

// snippetRadius is how much context to include either side of a match.
const snippetRadius = 60

// snippet extracts a short window around a match, trimmed to line-ish bounds so
// the caller sees usable context rather than a hard character cut.
func snippet(content string, idx, matchLen int) string {
	start := idx - snippetRadius
	if start < 0 {
		start = 0
	}
	end := idx + matchLen + snippetRadius
	if end > len(content) {
		end = len(content)
	}
	s := content[start:end]
	if start > 0 {
		s = "…" + s
	}
	if end < len(content) {
		s += "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}
