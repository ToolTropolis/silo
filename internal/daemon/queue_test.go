package daemon

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
)

// mapBackend is a multi-path in-memory DurableBackend for sync tests. It can be
// toggled "down" (every op returns a connection error) to model an outage.
type mapBackend struct {
	mu    sync.Mutex
	objs  map[string][]byte
	etags map[string]int
	down  bool
	gets  int // counts Get calls, so tests can assert "no backend traffic"
}

// getCalls reports how many Gets have been made, for asserting that an idle
// sync pass costs no network traffic.
func (m *mapBackend) getCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gets
}

func newMapBackend() *mapBackend {
	return &mapBackend{objs: map[string][]byte{}, etags: map[string]int{}}
}

func (m *mapBackend) setDown(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down = down
}

var errBackendDown = errors.New("backend down")

func (m *mapBackend) key(projectID, path string) string { return projectID + "/" + path }

func (m *mapBackend) Get(_ context.Context, projectID, path, _ string) ([]byte, backend.ObjectVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	if m.down {
		return nil, backend.ObjectVersion{}, errBackendDown
	}
	k := m.key(projectID, path)
	v, ok := m.objs[k]
	if !ok {
		return nil, backend.ObjectVersion{}, backend.ErrNotFound
	}
	return append([]byte(nil), v...), backend.ObjectVersion{ETag: etag(m.etags[k])}, nil
}

func (m *mapBackend) Put(_ context.Context, projectID, path string, content []byte, opts backend.PutOptions) (backend.ObjectVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return backend.ObjectVersion{}, errBackendDown
	}
	k := m.key(projectID, path)
	if opts.IfMatchETag != "" && opts.IfMatchETag != etag(m.etags[k]) {
		return backend.ObjectVersion{}, backend.ErrPreconditionFailed
	}
	m.etags[k]++
	m.objs[k] = append([]byte(nil), content...)
	return backend.ObjectVersion{ETag: etag(m.etags[k])}, nil
}

func (m *mapBackend) ListVersions(context.Context, string, string) ([]backend.ObjectVersion, error) {
	return nil, nil
}

func (m *mapBackend) ListPaths(_ context.Context, projectID, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil, errBackendDown
	}
	want := projectID + "/"
	var out []string
	for k := range m.objs {
		if !strings.HasPrefix(k, want) {
			continue
		}
		p := strings.TrimPrefix(k, want)
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out) // deterministic for tests
	return out, nil
}
func (m *mapBackend) Delete(context.Context, string, string) error { return nil }
func (m *mapBackend) CreateBucket(context.Context, string) error   { return nil }
func (m *mapBackend) DeleteBucket(context.Context, string) error   { return nil }

func etag(n int) string { return "e" + string(rune('0'+n)) }

func newSyncDaemon(t *testing.T, b backend.DurableBackend) (*Daemon, cache.LocalCache) {
	t.Helper()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return New(b, c, newGenRegistry(), nil), c
}

// TestSyncProject_DrainsQueuedWritesAfterRecovery is the NAV-72 acceptance
// criterion: writes taken while the backend is down are queued locally, then
// synced with no loss once the backend recovers.
func TestSyncProject_DrainsQueuedWritesAfterRecovery(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, _ := newSyncDaemon(t, be)
	const proj = "proj-11"

	// Backend goes down; three writes are taken and must queue (not fail).
	be.setDown(true)
	writes := map[string]string{"a.md": "AAA", "b.md": "BBB", "c.md": "CCC"}
	for path, content := range writes {
		c := content
		if _, err := d.SafeWrite(ctx, proj, path, func([]byte) []byte { return []byte(c) }, "agent", "s1"); err != nil {
			t.Fatalf("SafeWrite while down should queue, got: %v", err)
		}
	}
	// Backend still down: SyncProject must refuse to drain (so it can't lose them).
	if err := d.SyncProject(ctx, proj); err == nil {
		t.Fatal("SyncProject should fail while backend is down")
	}

	// Backend recovers; sync drains the queue into the backend.
	be.setDown(false)
	if err := d.SyncProject(ctx, proj); err != nil {
		t.Fatalf("SyncProject after recovery: %v", err)
	}

	// All three writes are now durably present with the queued content.
	for path, want := range writes {
		got, _, err := be.Get(ctx, proj, path, "")
		if err != nil {
			t.Fatalf("%s missing after sync: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("%s: want %q, got %q", path, want, got)
		}
	}

	// The queue is empty; a second sync is a no-op.
	if err := d.SyncProject(ctx, proj); err != nil {
		t.Fatalf("second SyncProject: %v", err)
	}
}

// TestSyncProject_EmptyQueue is a no-op and must not error.
func TestSyncProject_EmptyQueue(t *testing.T) {
	be := newMapBackend()
	d, _ := newSyncDaemon(t, be)
	if err := d.SyncProject(context.Background(), "proj-x"); err != nil {
		t.Fatalf("SyncProject on empty queue: %v", err)
	}
}

// TestSyncProject_ReEnqueuesOnReplayFailure confirms a mid-drain failure doesn't
// drop writes: they go back on the queue for a later retry.
func TestSyncProject_ReEnqueuesOnReplayFailure(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	d, c := newSyncDaemon(t, be)
	const proj = "proj-9"

	// Queue two writes while down.
	be.setDown(true)
	for _, p := range []string{"x.md", "y.md"} {
		if _, err := d.SafeWrite(ctx, proj, p, func([]byte) []byte { return []byte("v") }, "a", "s"); err != nil {
			t.Fatalf("enqueue %s: %v", p, err)
		}
	}

	// Backend passes the reachability probe (Get works) but every Put fails,
	// simulating a flaky recovery. We model this with a wrapper.
	flaky := &putFailBackend{mapBackend: be}
	be.setDown(false)
	d2 := New(flaky, c, nil, nil)

	if err := d2.SyncProject(ctx, proj); err == nil {
		t.Fatal("expected replay failure")
	}
	// Both writes must still be queued (re-enqueued), not lost.
	pending, err := c.DrainQueue(ctx, proj)
	if err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 re-enqueued writes, got %d", len(pending))
	}
}

// putFailBackend passes Get (reachability) but fails every Put.
type putFailBackend struct {
	*mapBackend
}

func (p *putFailBackend) Put(context.Context, string, string, []byte, backend.PutOptions) (backend.ObjectVersion, error) {
	return backend.ObjectVersion{}, errors.New("put rejected")
}

// failAfterNPuts goes down permanently once it has accepted n Puts, modelling a
// backend that dies partway through a drain.
type failAfterNPuts struct {
	*mapBackend
	mu        sync.Mutex
	remaining int
}

func (f *failAfterNPuts) Put(ctx context.Context, projectID, path string, content []byte, opts backend.PutOptions) (backend.ObjectVersion, error) {
	f.mu.Lock()
	if f.remaining <= 0 {
		f.mu.Unlock()
		f.mapBackend.setDown(true)
		return backend.ObjectVersion{}, errBackendDown
	}
	f.remaining--
	f.mu.Unlock()
	return f.mapBackend.Put(ctx, projectID, path, content, opts)
}

// TestSyncProject_DoesNotDoubleEnqueueOnMidDrainOutage guards the most likely
// way to corrupt the queue while wiring the sync worker.
//
// SyncProject replays through SafeWrite, and SafeWrite ALSO enqueues when it
// finds the backend unreachable. So a backend that dies mid-drain can have the
// same write buffered twice: once by SafeWrite's own fallback and once by
// SyncProject re-enqueueing the remainder. The queue must end up holding each
// unsynced write exactly once.
func TestSyncProject_DoesNotDoubleEnqueueOnMidDrainOutage(t *testing.T) {
	ctx := context.Background()
	inner := newMapBackend()
	be := &failAfterNPuts{mapBackend: inner, remaining: 1} // 1st replay lands, then it dies
	d, c := newSyncDaemon(t, be)
	const proj = "proj-11"

	// Queue three writes while the backend is down.
	inner.setDown(true)
	for _, p := range []string{"memory/a.md", "memory/b.md", "memory/c.md"} {
		if _, err := d.SafeWrite(ctx, proj, p, func([]byte) []byte { return []byte(p) }, "agent", "s1"); err != nil {
			t.Fatalf("queueing write %s: %v", p, err)
		}
	}
	if depth, _ := c.QueueDepth(ctx, proj); depth != 3 {
		t.Fatalf("setup: queue depth = %d, want 3", depth)
	}

	// Backend "recovers" enough to start the drain, then dies after one Put.
	inner.setDown(false)
	if err := d.SyncProject(ctx, proj); err == nil {
		t.Fatal("a mid-drain outage should surface as an error")
	}

	// One write landed; two remain. Not three (the failed one counted twice),
	// and not four (SafeWrite's fallback plus SyncProject's re-enqueue).
	depth, err := c.QueueDepth(ctx, proj)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 2 {
		t.Errorf("queue depth after mid-drain outage = %d, want 2 "+
			"(each unsynced write buffered exactly once)", depth)
	}
}
