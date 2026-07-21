package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
)

// fakeBackend is an in-memory DurableBackend that models ETag CAS, so SafeWrite
// can be exercised deterministically without SeaweedFS. A test hook lets us
// inject a concurrent write in the window between Get and Put.
type fakeBackend struct {
	mu       sync.Mutex
	content  []byte
	etag     string
	exists   bool
	nextETag int

	// beforePut, if set, runs inside Put before the CAS check on the FIRST call
	// only — simulating another writer landing in the Get/Put gap.
	beforePut func()
	putCalls  int
	getErr    error // if set, Get returns this (models backend unreachable)
}

func (f *fakeBackend) Get(ctx context.Context, projectID, path, versionID string) ([]byte, backend.ObjectVersion, error) {
	if f.getErr != nil {
		return nil, backend.ObjectVersion{}, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return nil, backend.ObjectVersion{}, backend.ErrNotFound
	}
	return append([]byte(nil), f.content...), backend.ObjectVersion{ETag: f.etag}, nil
}

func (f *fakeBackend) Put(ctx context.Context, projectID, path string, content []byte, opts backend.PutOptions) (backend.ObjectVersion, error) {
	f.mu.Lock()
	f.putCalls++
	first := f.putCalls == 1
	hook := f.beforePut
	f.mu.Unlock()

	if first && hook != nil {
		hook() // a competing writer lands here, moving the ETag
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if opts.IfMatchETag != "" && opts.IfMatchETag != f.etag {
		return backend.ObjectVersion{}, backend.ErrPreconditionFailed
	}
	f.nextETag++
	f.etag = etagN(f.nextETag)
	f.content = append([]byte(nil), content...)
	f.exists = true
	return backend.ObjectVersion{ETag: f.etag}, nil
}

func (f *fakeBackend) ListVersions(context.Context, string, string) ([]backend.ObjectVersion, error) {
	return nil, nil
}
func (f *fakeBackend) Delete(context.Context, string, string) error { return nil }
func (f *fakeBackend) CreateBucket(context.Context, string) error   { return nil }
func (f *fakeBackend) DeleteBucket(context.Context, string) error   { return nil }

// directPut sets content bypassing CAS, used by the injected competing writer.
func (f *fakeBackend) directPut(content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextETag++
	f.etag = etagN(f.nextETag)
	f.content = append([]byte(nil), content...)
	f.exists = true
}

func etagN(n int) string { return "etag-" + string(rune('0'+n)) }

func newTestDaemon(t *testing.T, b backend.DurableBackend) *Daemon {
	t.Helper()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return New(b, c, nil, nil)
}

// TestSafeWrite_RetriesOnConflict forces a concurrent-write conflict in the
// Get/Put window and confirms SafeWrite retries and succeeds rather than
// silently overwriting — the v1 definition-of-done case.
func TestSafeWrite_RetriesOnConflict(t *testing.T) {
	fb := &fakeBackend{}
	// Seed an initial object.
	fb.directPut([]byte("base"))

	// On the first Put attempt, a competing writer lands first, invalidating the
	// ETag SafeWrite read — the first Put must hit ErrPreconditionFailed and retry.
	fb.beforePut = func() {
		fb.directPut([]byte("competitor"))
		fb.beforePut = nil // only interfere once
	}

	d := newTestDaemon(t, fb)
	err := d.SafeWrite(context.Background(), "proj-11", "notes.md",
		func(cur []byte) []byte { return append(append([]byte(nil), cur...), []byte(" +mine")...) },
		"agent-1", "s1")
	if err != nil {
		t.Fatalf("SafeWrite: want success after retry, got %v", err)
	}
	if fb.putCalls < 2 {
		t.Fatalf("expected a retry (>=2 Put calls), got %d", fb.putCalls)
	}
	// The final content must be built on the competitor's value, not the stale base.
	got, _, _ := fb.Get(context.Background(), "proj-11", "notes.md", "")
	if string(got) != "competitor +mine" {
		t.Fatalf("retry didn't rebuild on latest: got %q", got)
	}
}

// TestSafeWrite_QueuesWhenBackendDown confirms an unreachable backend routes the
// write to the local cache queue instead of failing or losing it.
func TestSafeWrite_QueuesWhenBackendDown(t *testing.T) {
	fb := &fakeBackend{getErr: errors.New("connection refused")}
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	d := New(fb, c, nil, nil)

	err = d.SafeWrite(context.Background(), "proj-11", "notes.md",
		func([]byte) []byte { return []byte("queued content") }, "agent-1", "s1")
	if err != nil {
		t.Fatalf("SafeWrite with backend down: want nil (queued), got %v", err)
	}

	pending, err := c.DrainQueue(context.Background(), "proj-11")
	if err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}
	if len(pending) != 1 || string(pending[0].Content) != "queued content" {
		t.Fatalf("expected one queued write, got %+v", pending)
	}
}
