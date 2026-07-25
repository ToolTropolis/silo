package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestCompact_ReclaimsDisk is the reason compaction exists: eviction frees pages
// for reuse, but the file itself never shrinks, so a project that cached a lot
// and then evicted most of it holds disk it is not using.
func TestCompact_ReclaimsDisk(t *testing.T) {
	ctx := context.Background()
	c, _ := newClockedCache(t)

	// Enough data that the file grows well past its initial page allocation.
	big := make([]byte, 8*1024)
	for i := range 200 {
		if err := c.Put(ctx, testProject, fmt.Sprintf("memory/%03d.md", i), big); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	// Keep only a handful.
	if _, err := c.Evict(ctx, testProject, EvictPolicy{MaxEntries: 5}); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	before, err := c.fileInfo(testProject)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	res, err := c.Compact(ctx, testProject)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.Skipped {
		t.Fatalf("compaction was skipped: %s", res.SkipReason)
	}

	after, err := c.fileInfo(testProject)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if after >= before {
		t.Errorf("file did not shrink: %d -> %d bytes", before, after)
	}
	if res.Reclaimed() <= 0 {
		t.Errorf("reclaimed %d bytes, want > 0", res.Reclaimed())
	}

	// Survivors must read back byte-identical — compaction that loses or
	// corrupts data would be far worse than the disk it saves.
	stats, err := c.Stats(ctx, testProject)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Entries != 5 {
		t.Errorf("entries after compaction = %d, want 5", stats.Entries)
	}
	for i := 195; i < 200; i++ {
		got, err := c.Get(ctx, testProject, fmt.Sprintf("memory/%03d.md", i))
		if err != nil {
			t.Errorf("surviving entry %d unreadable: %v", i, err)
			continue
		}
		if len(got) != len(big) {
			t.Errorf("entry %d is %d bytes, want %d", i, len(got), len(big))
		}
	}
}

// TestCompact_SkipsWithQueuedWrites: bbolt copies through a read transaction, so
// anything written during the copy is missing from the destination. Cache
// content survives that — the backend can supply it again — but an unsynced
// write exists nowhere else.
func TestCompact_SkipsWithQueuedWrites(t *testing.T) {
	ctx := context.Background()
	c, _ := newClockedCache(t)

	if err := c.Put(ctx, testProject, "memory/x.md", []byte("cached")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Enqueue(ctx, testProject, PendingWrite{Path: "memory/q.md", Content: []byte("unsynced")}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	before, _ := c.fileInfo(testProject)
	res, err := c.Compact(ctx, testProject)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !res.Skipped {
		t.Fatal("compaction must be skipped while writes are queued")
	}

	after, _ := c.fileInfo(testProject)
	if after != before {
		t.Errorf("the file was modified despite skipping: %d -> %d", before, after)
	}
	if depth, _ := c.QueueDepth(ctx, testProject); depth != 1 {
		t.Errorf("queue depth = %d, want 1 intact", depth)
	}
}

// The cache must stay usable after compaction — this is where a closed handle
// left in the map would break every later request for the project.
func TestCompact_LeavesAUsableHandle(t *testing.T) {
	ctx := context.Background()
	c, _ := newClockedCache(t)

	if err := c.Put(ctx, testProject, "memory/x.md", []byte("before")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := c.Compact(ctx, testProject); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if got, err := c.Get(ctx, testProject, "memory/x.md"); err != nil {
		t.Fatalf("read after compaction: %v", err)
	} else if string(got) != "before" {
		t.Errorf("got %q, want %q", got, "before")
	}
	if err := c.Put(ctx, testProject, "memory/y.md", []byte("after")); err != nil {
		t.Fatalf("write after compaction: %v", err)
	}
	if _, err := c.QueueDepth(ctx, testProject); err != nil {
		t.Errorf("queue unusable after compaction: %v", err)
	}
}

// The generation binding must survive, or the next read would wipe the cache it
// just compacted.
func TestCompact_PreservesTheGenerationBinding(t *testing.T) {
	ctx := context.Background()
	c, _ := newClockedCache(t)

	if err := c.Put(ctx, testProject, "memory/x.md", []byte("keep")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := c.Compact(ctx, testProject); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Re-binding the same generation must be a no-op, not a wipe.
	if err := c.BindProject(ctx, testProject, genA); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if _, err := c.Get(ctx, testProject, "memory/x.md"); err != nil {
		t.Errorf("content should survive compaction and a rebind: %v", err)
	}
}

// A project this host never served is not an error to compact.
func TestCompact_AbsentFileIsSkipped(t *testing.T) {
	c := newTestCache(t)
	res, err := c.Compact(context.Background(), "never-seen")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !res.Skipped {
		t.Error("compacting an absent file should be skipped, not attempted")
	}
}

func TestCompact_RejectsUnsafeProjectID(t *testing.T) {
	c := newTestCache(t)
	for _, bad := range []string{"../escape", "a/b", "Repo1", ""} {
		if _, err := c.Compact(context.Background(), bad); err == nil {
			t.Errorf("Compact(%q) should be rejected", bad)
		}
	}
}

// A crash before the rename leaves a complete but unreferenced copy. Left alone
// it doubles the project's disk usage indefinitely — the opposite of the point.
func TestNewBoltCache_SweepsCompactLeftovers(t *testing.T) {
	dir := t.TempDir()
	leftover := filepath.Join(dir, "proj-a.bbolt"+compactSuffix)
	if err := os.WriteFile(leftover, []byte("interrupted"), 0o600); err != nil {
		t.Fatalf("seed leftover: %v", err)
	}

	c, err := NewBoltCache(dir)
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	defer c.Close()

	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("a leftover compaction file should be swept at startup, stat gave %v", err)
	}
}

// Reads during a compaction must not race or see a closed handle.
func TestCompact_ConcurrentReadsAreSafe(t *testing.T) {
	ctx := context.Background()
	c, _ := newClockedCache(t)

	for i := range 50 {
		if err := c.Put(ctx, testProject, fmt.Sprintf("memory/%02d.md", i), make([]byte, 512)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				// Errors are fine here (the handle is swapped mid-flight); a
				// data race or a panic is not, which is what -race catches.
				_, _ = c.Get(ctx, testProject, "memory/01.md")
				time.Sleep(time.Millisecond)
			}
		}
	}()

	if _, err := c.Compact(ctx, testProject); err != nil {
		t.Errorf("Compact: %v", err)
	}
	close(stop)
	wg.Wait()

	if _, err := c.Get(ctx, testProject, "memory/01.md"); err != nil {
		t.Errorf("cache should be usable after concurrent compaction: %v", err)
	}
}
