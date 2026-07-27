package cache

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// newClockedCache returns a cache whose clock the test controls, so eviction
// can be exercised without sleeping.
func newClockedCache(t *testing.T) (*BoltCache, *time.Time) {
	t.Helper()
	c := newTestCache(t)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	if err := c.BindProject(context.Background(), testProject, genA); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return c, &now
}

func TestEvict_TTL(t *testing.T) {
	ctx := context.Background()
	c, now := newClockedCache(t)

	if err := c.Put(ctx, testProject, "memory/old.md", []byte("old")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	*now = now.Add(2 * time.Hour)
	if err := c.Put(ctx, testProject, "memory/new.md", []byte("new")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, err := c.Evict(ctx, testProject, EvictPolicy{TTL: time.Hour})
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.EvictedTTL != 1 {
		t.Errorf("evicted %d by TTL, want 1", res.EvictedTTL)
	}
	if _, err := c.Get(ctx, testProject, "memory/old.md"); !errors.Is(err, ErrNotFound) {
		t.Error("the expired entry should be gone")
	}
	if _, err := c.Get(ctx, testProject, "memory/new.md"); err != nil {
		t.Errorf("the fresh entry should survive: %v", err)
	}
}

func TestEvict_MaxEntries(t *testing.T) {
	ctx := context.Background()
	c, now := newClockedCache(t)

	// Distinct write times so "oldest first" is well defined.
	for i := range 5 {
		if err := c.Put(ctx, testProject, fmt.Sprintf("memory/%d.md", i), []byte("x")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		*now = now.Add(time.Minute)
	}

	res, err := c.Evict(ctx, testProject, EvictPolicy{MaxEntries: 2})
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.EntriesAfter != 2 {
		t.Errorf("entries after = %d, want 2", res.EntriesAfter)
	}
	// The two newest survive.
	for _, keep := range []string{"memory/3.md", "memory/4.md"} {
		if _, err := c.Get(ctx, testProject, keep); err != nil {
			t.Errorf("%s should survive as one of the newest: %v", keep, err)
		}
	}
	if _, err := c.Get(ctx, testProject, "memory/0.md"); !errors.Is(err, ErrNotFound) {
		t.Error("the oldest entry should be evicted first")
	}
}

func TestEvict_MaxBytes(t *testing.T) {
	ctx := context.Background()
	c, now := newClockedCache(t)

	big := make([]byte, 1024)
	for i := range 4 {
		if err := c.Put(ctx, testProject, fmt.Sprintf("memory/%d.md", i), big); err != nil {
			t.Fatalf("Put: %v", err)
		}
		*now = now.Add(time.Minute)
	}

	// Room for roughly two entries.
	res, err := c.Evict(ctx, testProject, EvictPolicy{MaxBytes: 2200})
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.BytesAfter > 2200 {
		t.Errorf("bytes after = %d, want <= 2200", res.BytesAfter)
	}
	if res.EvictedSize == 0 {
		t.Error("expected the size cap to evict something")
	}
}

// TestEvict_NeverTouchesTheQueue is the property that matters most: the queue
// holds writes that never reached the backend, so evicting from it would be
// data loss rather than cache management.
func TestEvict_NeverTouchesTheQueue(t *testing.T) {
	ctx := context.Background()
	c, _ := newClockedCache(t)

	for i := range 3 {
		if err := c.Enqueue(ctx, testProject, PendingWrite{
			Path:    fmt.Sprintf("memory/queued%d.md", i),
			Content: []byte("unsynced"),
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if err := c.Put(ctx, testProject, "memory/plain.md", []byte("cached")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// The most aggressive policy expressible.
	if _, err := c.Evict(ctx, testProject, EvictPolicy{TTL: time.Nanosecond, MaxEntries: 1, MaxBytes: 1}); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	depth, err := c.QueueDepth(ctx, testProject)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 3 {
		t.Fatalf("queue depth = %d, want 3 — eviction must never remove unsynced writes", depth)
	}
	drained, err := c.DrainQueue(ctx, testProject)
	if err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}
	if len(drained) != 3 {
		t.Errorf("drained %d writes, want 3 intact", len(drained))
	}
}

// TestEvict_PinsQueuedPaths: the write path caches content for a queued write so
// an agent can read back what it just wrote during an outage. Evicting that
// while the queue entry survives would break exactly the offline case the cache
// exists for.
func TestEvict_PinsQueuedPaths(t *testing.T) {
	ctx := context.Background()
	c, now := newClockedCache(t)

	// A queued write, with its content cached the way SafeWrite does it.
	if err := c.Enqueue(ctx, testProject, PendingWrite{Path: "memory/pinned.md", Content: []byte("unsynced")}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := c.Put(ctx, testProject, "memory/pinned.md", []byte("unsynced")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Age it well past any TTL.
	*now = now.Add(30 * 24 * time.Hour)

	res, err := c.Evict(ctx, testProject, EvictPolicy{TTL: time.Hour, MaxEntries: 1})
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.Pinned != 1 {
		t.Errorf("pinned = %d, want 1", res.Pinned)
	}
	got, err := c.Get(ctx, testProject, "memory/pinned.md")
	if err != nil {
		t.Fatalf("a queued write's content must stay readable: %v", err)
	}
	if string(got) != "unsynced" {
		t.Errorf("got %q, want %q", got, "unsynced")
	}
}

// An unset policy must change nothing — the behaviour before eviction existed.
func TestEvict_ZeroPolicyEvictsNothing(t *testing.T) {
	ctx := context.Background()
	c, now := newClockedCache(t)

	for i := range 3 {
		if err := c.Put(ctx, testProject, fmt.Sprintf("memory/%d.md", i), []byte("x")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	*now = now.Add(365 * 24 * time.Hour)

	res, err := c.Evict(ctx, testProject, EvictPolicy{})
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.Evicted() != 0 {
		t.Errorf("evicted %d with an unset policy, want 0", res.Evicted())
	}
}

// Eviction in one project must not touch another's cache.
func TestEvict_IsolatedPerProject(t *testing.T) {
	ctx := context.Background()
	c := newTestCache(t)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	for _, p := range []string{"proj-a", "proj-b"} {
		if err := c.BindProject(ctx, p, genA); err != nil {
			t.Fatalf("bind %s: %v", p, err)
		}
		if err := c.Put(ctx, p, "memory/x.md", []byte(p)); err != nil {
			t.Fatalf("Put %s: %v", p, err)
		}
	}
	now = now.Add(2 * time.Hour)

	if _, err := c.Evict(ctx, "proj-a", EvictPolicy{TTL: time.Hour}); err != nil {
		t.Fatalf("Evict proj-a: %v", err)
	}
	if _, err := c.Get(ctx, "proj-a", "memory/x.md"); !errors.Is(err, ErrNotFound) {
		t.Error("proj-a's expired entry should be gone")
	}
	if _, err := c.Get(ctx, "proj-b", "memory/x.md"); err != nil {
		t.Errorf("proj-b must be untouched by proj-a's eviction: %v", err)
	}
}

func TestStats(t *testing.T) {
	ctx := context.Background()
	c, _ := newClockedCache(t)

	for i := range 3 {
		if err := c.Put(ctx, testProject, fmt.Sprintf("memory/%d.md", i), make([]byte, 100)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	s, err := c.Stats(ctx, testProject)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Entries != 3 {
		t.Errorf("entries = %d, want 3", s.Entries)
	}
	// Each entry is its content plus a header.
	if want := int64(3 * (100 + entryHeaderSz)); s.Bytes != want {
		t.Errorf("bytes = %d, want %d", s.Bytes, want)
	}
	if s.FileBytes <= 0 {
		t.Error("file size should be reported")
	}
}

// The entry header round-trips, and a value without one reads as corrupt rather
// than as content.
func TestEntryEncoding(t *testing.T) {
	written := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	encoded := encodeEntry([]byte("hello"), written)

	content, gotTime, err := decodeEntry(encoded)
	if err != nil {
		t.Fatalf("decodeEntry: %v", err)
	}
	if string(content) != "hello" {
		t.Errorf("content = %q, want %q", content, "hello")
	}
	if !gotTime.Equal(written) {
		t.Errorf("writtenAt = %v, want %v", gotTime, written)
	}

	for _, bad := range [][]byte{nil, []byte("short"), []byte("not-a-valid-silo-entry-at-all")} {
		if _, _, err := decodeEntry(bad); !errors.Is(err, ErrCorruptEntry) {
			t.Errorf("decodeEntry(%q) should report corruption, got %v", bad, err)
		}
	}
}
