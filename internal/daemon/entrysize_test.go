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

// fixedLimit is an EntryLimitSource with one cap for every project.
type fixedLimit int64

func (f fixedLimit) MaxEntryBytes(string) int64 { return int64(f) }

func newCappedDaemon(t *testing.T, be *fakeBackend, limit int64) *Daemon {
	t.Helper()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return New(be, c, newGenRegistry(), nil).WithEntryLimits(fixedLimit(limit))
}

func put(content string) func([]byte) []byte {
	return func([]byte) []byte { return []byte(content) }
}

// The cap has to actually refuse. Without it one agent writing a huge file
// blows out the cache and every later read of that path.
func TestEntrySize_RefusesAnOversizedWrite(t *testing.T) {
	d := newCappedDaemon(t, &fakeBackend{}, 100)

	_, err := d.SafeWrite(context.Background(), "proj-a", "memory/big.md",
		put(strings.Repeat("x", 101)), "agent", "")
	if !errors.Is(err, ErrEntryTooLarge) {
		t.Fatalf("SafeWrite over the cap = %v, want ErrEntryTooLarge", err)
	}
	// The message has to name both numbers: an agent told only "too large"
	// cannot tell whether to split the file in two or in fifty.
	for _, want := range []string{"101", "100"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should report the actual and permitted sizes", err)
		}
	}
}

// Nothing may reach the backend. A refusal that still created a version would
// be worse than no cap, since the oversized object would exist anyway.
func TestEntrySize_RefusedWriteNeverReachesTheBackend(t *testing.T) {
	be := &fakeBackend{}
	d := newCappedDaemon(t, be, 50)

	_, err := d.SafeWrite(context.Background(), "proj-a", "memory/big.md",
		put(strings.Repeat("x", 500)), "agent", "")
	if !errors.Is(err, ErrEntryTooLarge) {
		t.Fatalf("want ErrEntryTooLarge, got %v", err)
	}
	if be.exists {
		t.Error("the oversized write reached the backend and created a version")
	}
}

// Exactly at the cap is allowed: the limit is a maximum, and an off-by-one here
// would refuse a write an operator explicitly permitted.
func TestEntrySize_BoundaryIsInclusive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"one under", 99, false},
		{"exactly at the cap", 100, false},
		{"one over", 101, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newCappedDaemon(t, &fakeBackend{}, 100)
			_, err := d.SafeWrite(context.Background(), "proj-a", "memory/x.md",
				put(strings.Repeat("x", tc.size)), "agent", "")
			if tc.wantErr && !errors.Is(err, ErrEntryTooLarge) {
				t.Errorf("%d bytes against a 100-byte cap = %v, want refusal", tc.size, err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("%d bytes against a 100-byte cap = %v, want success", tc.size, err)
			}
		})
	}
}

// Zero means unlimited, matching how EvictPolicy reads its own zero values. An
// unconfigured daemon must not silently reject every write.
func TestEntrySize_ZeroMeansUnlimited(t *testing.T) {
	d := newCappedDaemon(t, &fakeBackend{}, 0)

	if _, err := d.SafeWrite(context.Background(), "proj-a", "memory/big.md",
		put(strings.Repeat("x", 10_000)), "agent", ""); err != nil {
		t.Errorf("a zero cap must mean unlimited, got %v", err)
	}
}

// A daemon with no limit source configured enforces nothing — the behaviour
// before this existed, which the quickstart depends on.
func TestEntrySize_NoLimitSourceEnforcesNothing(t *testing.T) {
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	d := New(&fakeBackend{}, c, newGenRegistry(), nil) // no WithEntryLimits

	if _, err := d.SafeWrite(context.Background(), "proj-a", "memory/big.md",
		put(strings.Repeat("x", 10_000)), "agent", ""); err != nil {
		t.Errorf("an unconfigured daemon must not enforce a cap, got %v", err)
	}
}

// The queued path is checked too. If the cap only applied when the backend was
// up, an outage would become the way around it — and the oversized entry would
// sit on local disk and be replayed into the bucket when the backend returned.
func TestEntrySize_EnforcedDuringABackendOutage(t *testing.T) {
	be := &fakeBackend{getErr: errors.New("backend unreachable")}
	d := newCappedDaemon(t, be, 100)
	ctx := context.Background()

	_, err := d.SafeWrite(ctx, "proj-a", "memory/big.md",
		put(strings.Repeat("x", 500)), "agent", "")
	if !errors.Is(err, ErrEntryTooLarge) {
		t.Fatalf("an outage must not bypass the cap, got %v", err)
	}

	// And nothing was queued for the sync worker to replay later.
	depth, err := d.QueueDepth(ctx, "proj-a")
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 0 {
		t.Errorf("queue depth = %d, want 0 — the refused write was buffered anyway", depth)
	}
}

// A write within the cap still queues normally during an outage: the check must
// not break the offline path it runs on.
func TestEntrySize_UnderCapStillQueuesDuringAnOutage(t *testing.T) {
	be := &fakeBackend{getErr: errors.New("backend unreachable")}
	d := newCappedDaemon(t, be, 100)
	ctx := context.Background()

	outcome, err := d.SafeWrite(ctx, "proj-a", "memory/small.md", put("fits"), "agent", "")
	if err != nil {
		t.Fatalf("SafeWrite under the cap during an outage: %v", err)
	}
	if outcome != WriteQueued {
		t.Errorf("outcome = %v, want WriteQueued", outcome)
	}
	depth, _ := d.QueueDepth(ctx, "proj-a")
	if depth != 1 {
		t.Errorf("queue depth = %d, want 1", depth)
	}
}

// 413, not 500: the request was understood and refused by policy. A 500 tells
// the caller to retry something that can never succeed, and hides a
// misconfigured cap as a server fault.
func TestEntrySize_HTTPReturns413(t *testing.T) {
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	d := New(&fakeBackend{}, c, newGenRegistry(), nil).WithEntryLimits(fixedLimit(100))
	h := NewServer(d, StaticTokenVerifier{"tok": "proj-a"}).Handler()

	body, _ := json.Marshal(writeRequest{
		Path:    "memory/big.md",
		Content: []byte(strings.Repeat("x", 500)),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/write", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized write = %d, want 413\nbody: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "limit") {
		t.Errorf("the response should explain the limit, got %q", rec.Body.String())
	}
}
