package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tooltropolis/silo/internal/cache"
)

func newPreconditionDaemon(t *testing.T, be *fakeBackend) *Daemon {
	t.Helper()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return New(be, c, newGenRegistry(), nil)
}

// The point of the feature: an agent editing content it read must not silently
// discard a change another agent made in between.
func TestPrecondition_StaleHashIsRefused(t *testing.T) {
	be := &fakeBackend{}
	be.directPut([]byte("original"))
	d := newPreconditionDaemon(t, be)

	staleHash := ContentHash([]byte("original"))

	// Another writer lands first.
	be.directPut([]byte("someone else's edit"))

	_, err := d.SafeWriteIfMatch(context.Background(), "proj-a", "memory/notes.md",
		put("my edit"), "agent", "", staleHash)
	if !errors.Is(err, ErrPreconditionMismatch) {
		t.Fatalf("a stale precondition = %v, want ErrPreconditionMismatch", err)
	}

	// And the other writer's content survives untouched.
	got, _, err := be.Get(context.Background(), "proj-a", "memory/notes.md", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "someone else's edit" {
		t.Errorf("stored content = %q — the refused write clobbered the other change", got)
	}
}

// A rejected precondition must create no version. Writing the same bytes back
// to satisfy the CAS loop would pollute the history with a no-op on every
// conflict.
func TestPrecondition_RefusalCreatesNoVersion(t *testing.T) {
	be := &fakeBackend{}
	be.directPut([]byte("original"))
	d := newPreconditionDaemon(t, be)

	// directPut seeds the object without going through Put, so the counter
	// reflects only what SafeWriteIfMatch does.
	before := be.putCalls

	_, err := d.SafeWriteIfMatch(context.Background(), "proj-a", "memory/notes.md",
		put("my edit"), "agent", "", ContentHash([]byte("something else entirely")))
	if !errors.Is(err, ErrPreconditionMismatch) {
		t.Fatalf("want ErrPreconditionMismatch, got %v", err)
	}
	if be.putCalls != before {
		t.Errorf("the backend saw %d Put call(s) for a refused write, want 0",
			be.putCalls-before)
	}
}

// A matching hash writes normally.
func TestPrecondition_MatchingHashSucceeds(t *testing.T) {
	be := &fakeBackend{}
	be.directPut([]byte("original"))
	d := newPreconditionDaemon(t, be)

	_, err := d.SafeWriteIfMatch(context.Background(), "proj-a", "memory/notes.md",
		put("my edit"), "agent", "", ContentHash([]byte("original")))
	if err != nil {
		t.Fatalf("a matching precondition should write: %v", err)
	}

	got, _, _ := be.Get(context.Background(), "proj-a", "memory/notes.md", "")
	if string(got) != "my edit" {
		t.Errorf("stored content = %q, want %q", got, "my edit")
	}
}

// An empty precondition is exactly SafeWrite, so every existing caller is
// unaffected.
func TestPrecondition_EmptyHashIsUnconditional(t *testing.T) {
	be := &fakeBackend{}
	be.directPut([]byte("original"))
	d := newPreconditionDaemon(t, be)

	if _, err := d.SafeWriteIfMatch(context.Background(), "proj-a", "memory/notes.md",
		put("overwrite"), "agent", "", ""); err != nil {
		t.Fatalf("an empty precondition should write unconditionally: %v", err)
	}
}

// The hash of absent content expresses "create only if nothing exists yet",
// which is what makes two agents creating the same path detectable.
func TestPrecondition_CreateOnlyIfAbsent(t *testing.T) {
	ctx := context.Background()
	absent := ContentHash(nil)

	t.Run("succeeds when the path is empty", func(t *testing.T) {
		d := newPreconditionDaemon(t, &fakeBackend{})
		if _, err := d.SafeWriteIfMatch(ctx, "proj-a", "memory/new.md",
			put("first"), "agent", "", absent); err != nil {
			t.Errorf("creating an absent path should succeed: %v", err)
		}
	})

	t.Run("refused when something is already there", func(t *testing.T) {
		be := &fakeBackend{}
		be.directPut([]byte("someone got here first"))
		d := newPreconditionDaemon(t, be)

		_, err := d.SafeWriteIfMatch(ctx, "proj-a", "memory/new.md",
			put("second"), "agent", "", absent)
		if !errors.Is(err, ErrPreconditionMismatch) {
			t.Errorf("creating an occupied path = %v, want ErrPreconditionMismatch", err)
		}
	})
}

// A precondition cannot be honoured against an unreachable backend: the cache
// may be stale, so a hash matching it proves nothing about what is stored.
// Silently dropping the check would let a caller believe a conflict check
// happened when none did.
func TestPrecondition_RefusedDuringABackendOutage(t *testing.T) {
	be := &fakeBackend{getErr: errors.New("backend unreachable")}
	d := newPreconditionDaemon(t, be)
	ctx := context.Background()

	_, err := d.SafeWriteIfMatch(ctx, "proj-a", "memory/notes.md",
		put("edit"), "agent", "", ContentHash([]byte("whatever")))
	if !errors.Is(err, ErrPreconditionMismatch) {
		t.Fatalf("a precondition during an outage = %v, want a refusal", err)
	}
	// Nothing may be queued: the caller has to know the check did not happen.
	if depth, _ := d.QueueDepth(ctx, "proj-a"); depth != 0 {
		t.Errorf("queue depth = %d, want 0 — the unverifiable write was buffered", depth)
	}

	// An unconditional write during the same outage still queues, so the check
	// has not broken the offline path.
	if _, err := d.SafeWriteIfMatch(ctx, "proj-a", "memory/notes.md",
		put("edit"), "agent", "", ""); err != nil {
		t.Fatalf("an unconditional write during an outage should queue: %v", err)
	}
	if depth, _ := d.QueueDepth(ctx, "proj-a"); depth != 1 {
		t.Errorf("queue depth = %d, want 1", depth)
	}
}

// The error names both hashes so an operator reading a log can see they differ.
func TestPrecondition_ErrorNamesBothHashes(t *testing.T) {
	be := &fakeBackend{}
	be.directPut([]byte("stored"))
	d := newPreconditionDaemon(t, be)

	expected := ContentHash([]byte("expected"))
	_, err := d.SafeWriteIfMatch(context.Background(), "proj-a", "memory/notes.md",
		put("x"), "agent", "", expected)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), short(expected)) {
		t.Errorf("error %q should name the expected hash", err)
	}
	if !strings.Contains(err.Error(), short(ContentHash([]byte("stored")))) {
		t.Errorf("error %q should name the actual hash", err)
	}
}

// ContentHash must be a plain SHA-256 of the bytes, not something backend- or
// version-specific: the SDK publishes HashOfAbsent as a constant, and a caller
// computing the hash itself has to get the same answer.
func TestContentHash_IsPlainSHA256(t *testing.T) {
	// Well-known SHA-256 of the empty input.
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	if got := ContentHash(nil); got != emptySHA256 {
		t.Errorf("ContentHash(nil) = %q, want the SHA-256 of no bytes", got)
	}
	if got := ContentHash([]byte{}); got != emptySHA256 {
		t.Errorf("ContentHash(empty) = %q, want the same as nil", got)
	}
	if ContentHash([]byte("a")) == ContentHash([]byte("b")) {
		t.Error("different content hashed to the same value")
	}
}

// The read response carries the hash a caller round-trips, and a stale one is a
// 409 rather than a 500 — the caller and the store disagree about current
// state, which is resolved by re-reading, not by retrying.
func TestPrecondition_HTTPRoundTripAndConflict(t *testing.T) {
	be := &fakeBackend{}
	be.directPut([]byte("original"))
	d := newPreconditionDaemon(t, be)
	h := NewServer(d, StaticTokenVerifier{"tok": "proj-a"}).Handler()

	// Read returns a usable hash.
	req := httptest.NewRequest(http.MethodGet, "/v1/read?path=memory/notes.md", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("read = %d, want 200", rec.Code)
	}
	var read readResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode read: %v", err)
	}
	if read.ContentSHA256 != ContentHash([]byte("original")) {
		t.Fatalf("content_sha256 = %q, want the hash of the stored content", read.ContentSHA256)
	}

	// Someone else writes.
	be.directPut([]byte("moved on"))

	// The round-tripped hash is now stale.
	body, _ := json.Marshal(writeRequest{
		Path:            "memory/notes.md",
		Content:         []byte("my edit"),
		IfContentSHA256: read.ContentSHA256,
	})
	wreq := httptest.NewRequest(http.MethodPost, "/v1/write", bytes.NewReader(body))
	wreq.Header.Set("Authorization", "Bearer tok")
	wrec := httptest.NewRecorder()
	h.ServeHTTP(wrec, wreq)

	if wrec.Code != http.StatusConflict {
		t.Errorf("stale precondition = %d, want 409\nbody: %s", wrec.Code, wrec.Body)
	}

	// A fresh hash succeeds.
	body, _ = json.Marshal(writeRequest{
		Path:            "memory/notes.md",
		Content:         []byte("my edit"),
		IfContentSHA256: ContentHash([]byte("moved on")),
	})
	wreq = httptest.NewRequest(http.MethodPost, "/v1/write", bytes.NewReader(body))
	wreq.Header.Set("Authorization", "Bearer tok")
	wrec = httptest.NewRecorder()
	h.ServeHTTP(wrec, wreq)
	if wrec.Code != http.StatusOK {
		t.Errorf("fresh precondition = %d, want 200\nbody: %s", wrec.Code, wrec.Body)
	}
}
