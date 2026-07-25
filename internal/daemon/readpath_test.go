package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
)

// TestRead_InvalidatesCacheOnBackendNotFound: a 404 is the backend positively
// stating the path is gone, so a cached copy is known-wrong. Left in place it
// would be served by the outage fallback later — resurrecting deleted content.
func TestRead_InvalidatesCacheOnBackendNotFound(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, c := newSyncDaemon(t, be)
	const proj, path = "proj-11", "memory/notes.md"

	// Warm the cache through a normal read.
	if _, err := d.SafeWrite(ctx, proj, path,
		func([]byte) []byte { return []byte("original") }, "agent", "s1"); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if _, err := c.Get(ctx, proj, path); err != nil {
		t.Fatalf("setup: cache should be warm: %v", err)
	}

	// The path disappears from the backend.
	be.mu.Lock()
	delete(be.objs, be.key(proj, path))
	be.mu.Unlock()

	if _, err := d.Read(ctx, proj, path); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read after backend delete: want ErrNotFound, got %v", err)
	}

	// The cached copy must be gone, not merely unused.
	if _, err := c.Get(ctx, proj, path); !errors.Is(err, cache.ErrNotFound) {
		t.Error("a backend 404 must invalidate the cached entry, or a later " +
			"outage would serve deleted content from the fallback")
	}
}

// The counterpart: an unreachable backend must NOT invalidate. That is the
// whole point of the cache, and dropping entries here would break offline reads.
func TestRead_PreservesCacheWhenBackendUnreachable(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, c := newSyncDaemon(t, be)
	const proj, path = "proj-11", "memory/notes.md"

	if _, err := d.SafeWrite(ctx, proj, path,
		func([]byte) []byte { return []byte("cached content") }, "agent", "s1"); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	be.setDown(true)

	got, err := d.Read(ctx, proj, path)
	if err != nil {
		t.Fatalf("Read during an outage should fall back to the cache: %v", err)
	}
	if string(got) != "cached content" {
		t.Errorf("read %q, want %q", got, "cached content")
	}
	if _, err := c.Get(ctx, proj, path); err != nil {
		t.Errorf("the cached entry must survive an outage: %v", err)
	}
}

// A 404 for one project must not disturb another's cache.
func TestRead_InvalidationIsPerProject(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, c := newSyncDaemon(t, be)
	const path = "memory/shared-name.md"

	for _, proj := range []string{"proj-a", "proj-b"} {
		if _, err := d.SafeWrite(ctx, proj, path,
			func([]byte) []byte { return []byte(proj) }, "agent", "s1"); err != nil {
			t.Fatalf("seed %s: %v", proj, err)
		}
	}

	// Delete only proj-a's copy from the backend.
	be.mu.Lock()
	delete(be.objs, be.key("proj-a", path))
	be.mu.Unlock()

	if _, err := d.Read(ctx, "proj-a", path); !errors.Is(err, ErrNotFound) {
		t.Fatalf("proj-a read: want ErrNotFound, got %v", err)
	}

	if _, err := c.Get(ctx, "proj-b", path); err != nil {
		t.Errorf("proj-b's cache entry must be untouched by proj-a's 404: %v", err)
	}
}

var _ backend.DurableBackend = (*mapBackend)(nil)
