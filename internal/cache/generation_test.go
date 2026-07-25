package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

const (
	genA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	genB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestBindProject_DiscardsAnotherGenerationsContent is the isolation guarantee.
//
// The cache file is named after the projectID, so a project torn down and
// re-onboarded under the same ID inherits the previous tenant's file. Binding
// as a new generation must leave nothing of the old one behind.
func TestBindProject_DiscardsAnotherGenerationsContent(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	c1, err := NewBoltCache(dir)
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	if err := c1.BindProject(ctx, testProject, genA); err != nil {
		t.Fatalf("bind genA: %v", err)
	}
	if err := c1.Put(ctx, testProject, "memory/secret.md", []byte("tenant A's memory")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A new tenant claims the same project ID.
	c2, err := NewBoltCache(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	if err := c2.BindProject(ctx, testProject, genB); err != nil {
		t.Fatalf("bind genB: %v", err)
	}

	if got, err := c2.Get(ctx, testProject, "memory/secret.md"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a new generation read the previous tenant's memory: %q (err=%v)", got, err)
	}
}

// The cross-tenant WRITE case: a stale queue must never replay into whoever
// next claims the project ID.
func TestBindProject_DiscardsAnotherGenerationsQueue(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	c1, err := NewBoltCache(dir)
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	if err := c1.BindProject(ctx, testProject, genA); err != nil {
		t.Fatalf("bind genA: %v", err)
	}
	for range 3 {
		if err := c1.Enqueue(ctx, testProject, PendingWrite{Path: "memory/a.md", Content: []byte("A")}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	c1.Close()

	c2, err := NewBoltCache(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	if err := c2.BindProject(ctx, testProject, genB); err != nil {
		t.Fatalf("bind genB: %v", err)
	}

	depth, err := c2.QueueDepth(ctx, testProject)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 0 {
		t.Errorf("queue depth = %d, want 0 — a previous tenant's writes must never "+
			"replay into the project that next claims this ID", depth)
	}
}

// Rebinding the same generation must be a no-op — the common case on every
// read and write.
func TestBindProject_SameGenerationPreservesEverything(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	c, err := NewBoltCache(dir)
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	defer c.Close()

	if err := c.BindProject(ctx, testProject, genA); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := c.Put(ctx, testProject, "memory/x.md", []byte("keep me")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Enqueue(ctx, testProject, PendingWrite{Path: "memory/x.md"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Bind repeatedly, as the read and write paths do.
	for range 3 {
		if err := c.BindProject(ctx, testProject, genA); err != nil {
			t.Fatalf("rebind: %v", err)
		}
	}

	got, err := c.Get(ctx, testProject, "memory/x.md")
	if err != nil {
		t.Fatalf("content should survive a rebind: %v", err)
	}
	if string(got) != "keep me" {
		t.Errorf("content = %q, want %q", got, "keep me")
	}
	if depth, _ := c.QueueDepth(ctx, testProject); depth != 1 {
		t.Errorf("queue depth = %d, want 1 — a rebind must not drop unsynced writes", depth)
	}
}

// A file written before generations existed can't be shown to belong to this
// tenant, so its content goes — but its QUEUE is this project's own unsynced
// data and dropping it would be data loss.
func TestBindProject_LegacyFileKeepsQueueDropsContent(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Hand-build a pre-generation file: content + queue, no meta stamp.
	path := filepath.Join(dir, testProject+".bbolt")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		cb, err := tx.CreateBucketIfNotExists(contentBucket)
		if err != nil {
			return err
		}
		if err := cb.Put([]byte("memory/old.md"), []byte("legacy content")); err != nil {
			return err
		}
		qb, err := tx.CreateBucketIfNotExists(queueBucket)
		if err != nil {
			return err
		}
		seq, _ := qb.NextSequence()
		return qb.Put(itob(seq), []byte(`{"Path":"memory/old.md","Content":"","Actor":"a"}`))
	})
	if err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}
	db.Close()

	c, err := NewBoltCache(dir)
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	defer c.Close()
	if err := c.BindProject(ctx, testProject, genA); err != nil {
		t.Fatalf("bind: %v", err)
	}

	if _, err := c.Get(ctx, testProject, "memory/old.md"); !errors.Is(err, ErrNotFound) {
		t.Error("unstamped content cannot be proven to belong to this tenant and must be discarded")
	}
	depth, err := c.QueueDepth(ctx, testProject)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 1 {
		t.Errorf("queue depth = %d, want 1 — an unstamped queue is this project's "+
			"own unsynced data and dropping it would be data loss", depth)
	}
}

// Binding with no generation must fail rather than match every file.
func TestBindProject_RejectsEmptyGeneration(t *testing.T) {
	c := newTestCache(t)
	err := c.BindProject(context.Background(), testProject, "")
	if !errors.Is(err, ErrNoGeneration) {
		t.Errorf("want ErrNoGeneration, got %v", err)
	}
}

// A generation change while the handle is live must close and re-check it
// rather than keep serving the old tenant.
func TestBindProject_RebindsLiveHandleOnGenerationChange(t *testing.T) {
	ctx := context.Background()
	c := newTestCache(t)

	if err := c.BindProject(ctx, testProject, genA); err != nil {
		t.Fatalf("bind genA: %v", err)
	}
	if err := c.Put(ctx, testProject, "memory/x.md", []byte("A")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// No Close — the handle stays open, as it would in a running daemon.
	if err := c.BindProject(ctx, testProject, genB); err != nil {
		t.Fatalf("bind genB: %v", err)
	}
	if _, err := c.Get(ctx, testProject, "memory/x.md"); !errors.Is(err, ErrNotFound) {
		t.Error("a live handle must be re-checked when the generation changes")
	}
}

// Binding one project must not touch another's file.
func TestBindProject_IsolatedPerProject(t *testing.T) {
	ctx := context.Background()
	c := newTestCache(t)

	for _, p := range []string{"proj-a", "proj-b"} {
		if err := c.BindProject(ctx, p, genA); err != nil {
			t.Fatalf("bind %s: %v", p, err)
		}
		if err := c.Put(ctx, p, "memory/x.md", []byte(p)); err != nil {
			t.Fatalf("Put %s: %v", p, err)
		}
	}

	// proj-a is re-onboarded; proj-b is untouched.
	if err := c.BindProject(ctx, "proj-a", genB); err != nil {
		t.Fatalf("rebind proj-a: %v", err)
	}
	got, err := c.Get(ctx, "proj-b", "memory/x.md")
	if err != nil {
		t.Fatalf("proj-b must be unaffected: %v", err)
	}
	if string(got) != "proj-b" {
		t.Errorf("proj-b content = %q, want %q", got, "proj-b")
	}
}

// Concurrent binds must not race or leak handles.
func TestBindProject_ConcurrentIsSafe(t *testing.T) {
	ctx := context.Background()
	c := newTestCache(t)

	done := make(chan error, 8)
	for range 8 {
		go func() { done <- c.BindProject(ctx, testProject, genA) }()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Errorf("concurrent bind: %v", err)
		}
	}

	// Exactly one file, and it still works.
	entries, err := os.ReadDir(c.baseDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d cache files, want 1", len(entries))
	}
	if err := c.Put(ctx, testProject, "memory/x.md", []byte("ok")); err != nil {
		t.Errorf("cache unusable after concurrent binds: %v", err)
	}
}
