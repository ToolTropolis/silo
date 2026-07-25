package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const testProject = "proj-11"

func newTestCache(t *testing.T) *BoltCache {
	t.Helper()
	c, err := NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestPendingWriteZeroValue(t *testing.T) {
	var w PendingWrite
	if w.Path != "" || w.Content != nil {
		t.Fatal("zero-value PendingWrite should be empty")
	}
}

func TestPutGetDelete(t *testing.T) {
	ctx := context.Background()
	c := newTestCache(t)

	// Get on a missing key -> ErrNotFound.
	if _, err := c.Get(ctx, testProject, "memory/notes.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: want ErrNotFound, got %v", err)
	}

	want := []byte("# Notes\n\nremember this")
	if err := c.Put(ctx, testProject, "memory/notes.md", want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := c.Get(ctx, testProject, "memory/notes.md")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Get: want %q, got %q", want, got)
	}

	// Overwrite.
	next := []byte("# Notes v2")
	if err := c.Put(ctx, testProject, "memory/notes.md", next); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}
	got, _ = c.Get(ctx, testProject, "memory/notes.md")
	if string(got) != string(next) {
		t.Fatalf("overwrite: want %q, got %q", next, got)
	}

	// Delete, then Get -> ErrNotFound.
	if err := c.Delete(ctx, testProject, "memory/notes.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Get(ctx, testProject, "memory/notes.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
	// Deleting a missing key is a no-op.
	if err := c.Delete(ctx, testProject, "memory/notes.md"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestProjectIsolation(t *testing.T) {
	ctx := context.Background()
	c := newTestCache(t)

	if err := c.Put(ctx, "proj-a", "shared/path.md", []byte("A's data")); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	// Same path, different project, must not see A's data.
	if _, err := c.Get(ctx, "proj-b", "shared/path.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project read leaked: want ErrNotFound, got %v", err)
	}
}

func TestEnqueueDrainFIFO(t *testing.T) {
	ctx := context.Background()
	c := newTestCache(t)

	// Empty queue drains to nothing.
	got, err := c.DrainQueue(ctx, testProject)
	if err != nil {
		t.Fatalf("DrainQueue empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty queue: want 0 writes, got %d", len(got))
	}

	paths := []string{"a.md", "b.md", "c.md"}
	for _, p := range paths {
		if err := c.Enqueue(ctx, testProject, PendingWrite{Path: p, Content: []byte(p), Actor: "agent-1"}); err != nil {
			t.Fatalf("Enqueue %s: %v", p, err)
		}
	}

	drained, err := c.DrainQueue(ctx, testProject)
	if err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}
	if len(drained) != len(paths) {
		t.Fatalf("drain count: want %d, got %d", len(paths), len(drained))
	}
	for i, p := range paths {
		if drained[i].Path != p {
			t.Fatalf("FIFO order broken at %d: want %q, got %q", i, p, drained[i].Path)
		}
		if drained[i].QueuedAt == "" {
			t.Fatalf("QueuedAt not stamped for %q", p)
		}
	}

	// Queue is empty after draining.
	again, err := c.DrainQueue(ctx, testProject)
	if err != nil {
		t.Fatalf("DrainQueue second: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("queue not cleared: got %d writes", len(again))
	}
}

func TestReopenPersistsData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	c1, err := NewBoltCache(dir)
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	if err := c1.Put(ctx, testProject, "keep.md", []byte("durable")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen the same dir — data survives.
	c2, err := NewBoltCache(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	got, err := c2.Get(ctx, testProject, "keep.md")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if string(got) != "durable" {
		t.Fatalf("persisted content: want %q, got %q", "durable", got)
	}
}

// TestBoltCache_RejectsUnsafeProjectID is the traversal guard. projectID is
// concatenated into a filename, so an ID like "../escape" would create a bbolt
// file outside the cache directory entirely. Assert both that the call fails
// and that nothing was written outside the base dir.
func TestBoltCache_RejectsUnsafeProjectID(t *testing.T) {
	parent := t.TempDir()
	baseDir := filepath.Join(parent, "cache")

	c, err := NewBoltCache(baseDir)
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	for _, bad := range []string{"../escape", "..", "a/b", "Repo1", "", "a_b"} {
		if err := c.Put(ctx, bad, "memory/x.md", []byte("nope")); err == nil {
			t.Errorf("Put with projectID %q should be rejected", bad)
		}
		if _, err := c.Get(ctx, bad, "memory/x.md"); err == nil {
			t.Errorf("Get with projectID %q should be rejected", bad)
		}
	}

	// Nothing may exist outside the cache dir — a created file here would mean
	// the ID escaped despite the error.
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "cache" {
			t.Errorf("traversal created %q outside the cache directory", e.Name())
		}
	}
}
