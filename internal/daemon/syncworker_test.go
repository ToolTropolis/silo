package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
)

// queueWrite buffers a write by taking it while the backend is down.
func queueWrite(t *testing.T, d *Daemon, be *mapBackend, projectID, path string) {
	t.Helper()
	be.setDown(true)
	if err := d.SafeWrite(context.Background(), projectID, path,
		func([]byte) []byte { return []byte(path) }, "agent", "s1"); err != nil {
		t.Fatalf("queueing %s: %v", path, err)
	}
}

// TestSyncWorker_DrainsOnRecovery is the whole point of the worker: writes taken
// during an outage must reach the backend once it returns, without anyone
// running a command.
func TestSyncWorker_DrainsOnRecovery(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, c := newSyncDaemon(t, be)
	const proj = "proj-11"

	for _, p := range []string{"memory/a.md", "memory/b.md", "memory/c.md"} {
		queueWrite(t, d, be, proj, p)
	}
	if depth, _ := c.QueueDepth(ctx, proj); depth != 3 {
		t.Fatalf("setup: depth = %d, want 3", depth)
	}

	be.setDown(false)
	w := NewSyncWorker(d, []string{proj}, time.Millisecond, nil)
	results := w.SyncOnce(ctx)

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("sync failed: %v", results[0].Err)
	}
	if results[0].Drained != 3 || results[0].Remaining != 0 {
		t.Errorf("drained=%d remaining=%d, want 3 and 0", results[0].Drained, results[0].Remaining)
	}
	if depth, _ := c.QueueDepth(ctx, proj); depth != 0 {
		t.Errorf("queue depth after sync = %d, want 0", depth)
	}
	// The content must actually be durable, not merely dequeued.
	for _, p := range []string{"memory/a.md", "memory/b.md", "memory/c.md"} {
		got, err := d.Read(ctx, proj, p)
		if err != nil {
			t.Errorf("Read %s after sync: %v", p, err)
			continue
		}
		if string(got) != p {
			t.Errorf("Read %s = %q, want %q", p, got, p)
		}
	}
}

// TestSyncWorker_SkipsEmptyQueues: the steady state is "nothing to do", and it
// must cost no network traffic — otherwise the worker turns an idle fleet into
// constant backend load.
func TestSyncWorker_SkipsEmptyQueues(t *testing.T) {
	be := newMapBackend()
	d, _ := newSyncDaemon(t, be)

	w := NewSyncWorker(d, []string{"proj-a", "proj-b"}, time.Millisecond, nil)
	before := be.getCalls()
	results := w.SyncOnce(context.Background())

	if len(results) != 0 {
		t.Errorf("got %d results for empty queues, want 0", len(results))
	}
	if after := be.getCalls(); after != before {
		t.Errorf("made %d backend call(s) with empty queues, want 0", after-before)
	}
}

// TestSyncWorker_LeavesQueueIntactWhenBackendDown: a failed drain must not
// consume the queue. Losing buffered writes to a failed sync is the exact
// failure this whole change exists to prevent.
func TestSyncWorker_LeavesQueueIntactWhenBackendDown(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, c := newSyncDaemon(t, be)
	const proj = "proj-11"

	queueWrite(t, d, be, proj, "memory/a.md")
	queueWrite(t, d, be, proj, "memory/b.md")

	// Backend stays down through the sync attempt.
	w := NewSyncWorker(d, []string{proj}, time.Millisecond, nil)
	results := w.SyncOnce(ctx)

	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("expected a failed sync result, got %+v", results)
	}
	depth, err := c.QueueDepth(ctx, proj)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 2 {
		t.Errorf("queue depth after a failed sync = %d, want 2 (nothing may be dropped)", depth)
	}
}

// TestSyncWorker_BacksOffPerProject: a project whose backend is unreachable must
// not stall a healthy one. Backoff is per project for exactly that reason.
func TestSyncWorker_BacksOffPerProject(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, _ := newSyncDaemon(t, be)

	// Both projects have queued writes; the backend is down for both.
	queueWrite(t, d, be, "proj-a", "memory/a.md")
	queueWrite(t, d, be, "proj-b", "memory/b.md")

	w := NewSyncWorker(d, []string{"proj-a", "proj-b"}, time.Hour, nil)
	if results := w.SyncOnce(ctx); len(results) != 2 {
		t.Fatalf("first pass: got %d results, want 2", len(results))
	}

	// Both failed, so both are now backing off — with a 1h interval, neither is
	// retried on the next pass.
	if results := w.SyncOnce(ctx); len(results) != 0 {
		t.Errorf("second pass: got %d results, want 0 (both backing off)", len(results))
	}

	// Clearing one project's backoff must not clear the other's.
	delete(w.backoff, "proj-a")
	results := w.SyncOnce(ctx)
	if len(results) != 1 || results[0].ProjectID != "proj-a" {
		t.Errorf("third pass: got %+v, want only proj-a retried", results)
	}
}

// TestSyncWorker_StopsOnContextCancel: shutdown must be prompt, since the final
// drain runs after it.
func TestSyncWorker_StopsOnContextCancel(t *testing.T) {
	be := newMapBackend()
	d, _ := newSyncDaemon(t, be)
	w := NewSyncWorker(d, []string{"proj-11"}, 10*time.Millisecond, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after cancel")
	}
}

// TestSyncWorker_DoesNotCrossProjects is the isolation guarantee: draining one
// project must never write into another's bucket or touch its queue.
func TestSyncWorker_DoesNotCrossProjects(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, c := newSyncDaemon(t, be)

	queueWrite(t, d, be, "proj-a", "memory/secret.md")
	// proj-b has nothing queued.

	be.setDown(false)
	w := NewSyncWorker(d, []string{"proj-a", "proj-b"}, time.Millisecond, nil)
	w.SyncOnce(ctx)

	// proj-a's write landed in proj-a.
	if _, err := d.Read(ctx, "proj-a", "memory/secret.md"); err != nil {
		t.Errorf("proj-a should have its own write: %v", err)
	}
	// ...and nowhere near proj-b.
	if _, err := d.Read(ctx, "proj-b", "memory/secret.md"); err == nil {
		t.Error("proj-a's write leaked into proj-b")
	}
	if depth, _ := c.QueueDepth(ctx, "proj-b"); depth != 0 {
		t.Errorf("proj-b queue depth = %d, want 0 (untouched)", depth)
	}
}

// TestSyncWorker_ConcurrentWritesDuringDrain runs under -race: an agent writing
// while the worker drains must not corrupt the queue or lose data.
func TestSyncWorker_ConcurrentWritesDuringDrain(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, _ := newSyncDaemon(t, be)
	const proj = "proj-11"

	for i := range 5 {
		queueWrite(t, d, be, proj, "memory/queued"+string(rune('a'+i))+".md")
	}
	be.setDown(false)

	w := NewSyncWorker(d, []string{proj}, time.Millisecond, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		w.SyncOnce(ctx)
	}()
	go func() {
		defer wg.Done()
		for i := range 5 {
			_ = d.SafeWrite(ctx, proj, "memory/live"+string(rune('a'+i))+".md",
				func([]byte) []byte { return []byte("live") }, "agent", "s2")
		}
	}()
	wg.Wait()

	// Everything must end up durable: the drain finished, and live writes went
	// straight through since the backend was up.
	paths, err := d.List(ctx, proj, "memory/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(paths) != 10 {
		t.Errorf("got %d paths, want 10 (5 queued + 5 live)", len(paths))
	}
}

var _ cache.LocalCache = (*cache.BoltCache)(nil)
var _ backend.DurableBackend = (*mapBackend)(nil)

// TestStaticTokenVerifier_Projects: the sync worker derives its project list
// from here, so duplicates (several agents sharing a project) must collapse and
// the order must be stable for legible startup logs.
func TestStaticTokenVerifier_Projects(t *testing.T) {
	v := StaticTokenVerifier{
		"tok-reviewer": "repo1",
		"tok-testgen":  "repo1", // same project, different agent
		"tok-other":    "project2",
	}
	got := v.Projects()
	if len(got) != 2 {
		t.Fatalf("Projects() = %v, want 2 distinct projects", got)
	}
	if got[0] != "project2" || got[1] != "repo1" {
		t.Errorf("Projects() = %v, want sorted [project2 repo1]", got)
	}
}
