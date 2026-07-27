package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tooltropolis/silo/internal/cache"
)

// TestEvictWorker_AppliesThePolicy: the sweep bounds the cache without anyone
// running a command.
func TestEvictWorker_AppliesThePolicy(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, c := newSyncDaemon(t, be)
	const proj = "proj-11"

	for _, p := range []string{"memory/a.md", "memory/b.md", "memory/c.md"} {
		if _, err := d.SafeWrite(ctx, proj, p, func([]byte) []byte { return []byte("x") }, "agent", "s1"); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	w := NewEvictWorker(d,
		func() []string { return []string{proj} },
		func(string) cache.EvictPolicy { return cache.EvictPolicy{MaxEntries: 1} },
		time.Millisecond, nil)

	results := w.EvictOnce(ctx)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].EntriesAfter != 1 {
		t.Errorf("entries after = %d, want 1", results[0].EntriesAfter)
	}

	stats, err := c.(*cache.BoltCache).Stats(ctx, proj)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Entries != 1 {
		t.Errorf("cache holds %d entries, want 1", stats.Entries)
	}
}

// An unset policy must not even open the files — the default for anyone who
// hasn't configured retention.
func TestEvictWorker_SkipsUnlimitedPolicies(t *testing.T) {
	be := newMapBackend()
	d, _ := newSyncDaemon(t, be)

	w := NewEvictWorker(d,
		func() []string { return []string{"proj-a", "proj-b"} },
		func(string) cache.EvictPolicy { return cache.EvictPolicy{} },
		time.Millisecond, nil)

	if results := w.EvictOnce(context.Background()); len(results) != 0 {
		t.Errorf("got %d results for an unset policy, want 0", len(results))
	}
}

// The policy is consulted per pass, so a configuration change takes effect
// without restarting the daemon.
func TestEvictWorker_ReReadsPolicyEachPass(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, _ := newSyncDaemon(t, be)
	const proj = "proj-11"

	for _, p := range []string{"memory/a.md", "memory/b.md"} {
		if _, err := d.SafeWrite(ctx, proj, p, func([]byte) []byte { return []byte("x") }, "agent", "s1"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	policy := cache.EvictPolicy{}
	w := NewEvictWorker(d,
		func() []string { return []string{proj} },
		func(string) cache.EvictPolicy { return policy },
		time.Millisecond, nil)

	if results := w.EvictOnce(ctx); len(results) != 0 {
		t.Fatal("nothing should be evicted before the policy is set")
	}

	policy = cache.EvictPolicy{MaxEntries: 1}
	results := w.EvictOnce(ctx)
	if len(results) != 1 || results[0].EntriesAfter != 1 {
		t.Errorf("the new policy should apply without a restart, got %+v", results)
	}
}

// Eviction must never remove unsynced writes, whatever the policy says.
func TestEvictWorker_NeverEvictsQueuedWrites(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, _ := newSyncDaemon(t, be)
	const proj = "proj-11"

	be.setDown(true)
	for _, p := range []string{"memory/q1.md", "memory/q2.md"} {
		if _, err := d.SafeWrite(ctx, proj, p, func([]byte) []byte { return []byte("unsynced") }, "agent", "s1"); err != nil {
			t.Fatalf("queueing: %v", err)
		}
	}

	w := NewEvictWorker(d,
		func() []string { return []string{proj} },
		func(string) cache.EvictPolicy {
			return cache.EvictPolicy{TTL: time.Nanosecond, MaxEntries: 1, MaxBytes: 1}
		},
		time.Millisecond, nil)
	w.EvictOnce(ctx)

	depth, err := d.QueueDepth(ctx, proj)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 2 {
		t.Errorf("queue depth = %d, want 2 — eviction must not touch unsynced writes", depth)
	}
}

func TestEvictWorker_StopsOnContextCancel(t *testing.T) {
	be := newMapBackend()
	d, _ := newSyncDaemon(t, be)
	w := NewEvictWorker(d,
		func() []string { return []string{"proj-11"} },
		func(string) cache.EvictPolicy { return cache.EvictPolicy{MaxEntries: 1} },
		10*time.Millisecond, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after cancel")
	}
}

// TestEvictWorker_CompactsAtMostOnePerPass: compaction takes a full copy of the
// file, so a fleet all doing it in the same tick is a disk and IO spike for
// something that is never urgent.
func TestEvictWorker_CompactsAtMostOnePerPass(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, _ := newSyncDaemon(t, be)

	// Two projects, each bloated enough to qualify.
	big := make([]byte, 64*1024)
	for _, proj := range []string{"proj-a", "proj-b"} {
		for i := range 100 {
			if _, err := d.SafeWrite(ctx, proj, fmt.Sprintf("memory/%03d.md", i),
				func([]byte) []byte { return big }, "agent", "s1"); err != nil {
				t.Fatalf("seed %s: %v", proj, err)
			}
		}
	}

	var compacted int
	w := NewEvictWorker(d,
		func() []string { return []string{"proj-a", "proj-b"} },
		func(string) cache.EvictPolicy { return cache.EvictPolicy{MaxEntries: 2} },
		time.Millisecond,
		func(format string, args ...any) {
			if strings.Contains(format, "compact") {
				compacted++
			}
		})

	w.EvictOnce(ctx)
	if compacted > 1 {
		t.Errorf("compacted %d projects in one pass, want at most 1", compacted)
	}
}
