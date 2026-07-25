package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newAdminFixture serves the admin surface over httptest for one project.
func newAdminFixture(t *testing.T, d *Daemon, projects ...string) *httptest.Server {
	t.Helper()
	admin := NewAdminServer(d, func() []string { return projects })
	srv := httptest.NewServer(admin.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// The console renders the gap between live content and file size, since that
// gap is exactly what compaction reclaims. Reporting only queue depth would
// leave it unable to show why compaction is worth running.
func TestAdmin_CacheStatsReportsSizes(t *testing.T) {
	const proj = "proj-11"
	d := newTestDaemon(t, &fakeBackend{})
	ctx := context.Background()
	if _, err := d.SafeWrite(ctx, proj, "memory/a.md",
		func([]byte) []byte { return []byte("hello") }, "tester", "s1"); err != nil {
		t.Fatalf("SafeWrite: %v", err)
	}

	srv := newAdminFixture(t, d, proj)
	resp, err := http.Get(srv.URL + "/v1/admin/cache-stats")
	if err != nil {
		t.Fatalf("GET cache-stats: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Projects []cacheStat `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Projects) != 1 {
		t.Fatalf("got %d projects, want 1", len(body.Projects))
	}
	s := body.Projects[0]
	if s.Entries != 1 {
		t.Errorf("entries = %d, want 1", s.Entries)
	}
	if s.Bytes <= 0 {
		t.Errorf("bytes = %d, want the cached content counted", s.Bytes)
	}
	if s.FileBytes <= 0 {
		t.Errorf("file_bytes = %d, want the on-disk size reported", s.FileBytes)
	}
}

func TestAdmin_CompactCache(t *testing.T) {
	const proj = "proj-11"
	d := newTestDaemon(t, &fakeBackend{})
	ctx := context.Background()
	if _, err := d.SafeWrite(ctx, proj, "memory/a.md",
		func([]byte) []byte { return []byte("hello") }, "tester", "s1"); err != nil {
		t.Fatalf("SafeWrite: %v", err)
	}

	srv := newAdminFixture(t, d, proj)
	resp, err := http.Post(srv.URL+"/v1/admin/compact-cache?project="+proj, "", nil)
	if err != nil {
		t.Fatalf("POST compact-cache: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body compactResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Compacted {
		t.Errorf("compacted = false (%s), want a compaction to run", body.SkipReason)
	}
	if body.BytesBefore <= 0 || body.BytesAfter <= 0 {
		t.Errorf("sizes = %d -> %d, want both reported", body.BytesBefore, body.BytesAfter)
	}

	// The cache must still be usable — a closed handle left behind would break
	// every later request for the project.
	if _, err := d.Read(ctx, proj, "memory/a.md"); err != nil {
		t.Errorf("read after compaction over the admin socket: %v", err)
	}
}

// A skip is a 200 with a reason, not an error: refusing while writes are queued
// is the designed safe behaviour, and the operator needs to be told why rather
// than handed a failure to interpret.
func TestAdmin_CompactSkipIsNotAnError(t *testing.T) {
	const proj = "proj-11"
	// An unreachable backend sends the write to the offline queue.
	d := newTestDaemon(t, &fakeBackend{getErr: errors.New("connection refused")})
	ctx := context.Background()
	if _, err := d.SafeWrite(ctx, proj, "memory/q.md",
		func([]byte) []byte { return []byte("unsynced") }, "tester", "s1"); err != nil {
		t.Fatalf("SafeWrite: %v", err)
	}
	if depth, _ := d.QueueDepth(ctx, proj); depth == 0 {
		t.Fatal("expected the write to be queued")
	}

	srv := newAdminFixture(t, d, proj)
	resp, err := http.Post(srv.URL+"/v1/admin/compact-cache?project="+proj, "", nil)
	if err != nil {
		t.Fatalf("POST compact-cache: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a safe skip", resp.StatusCode)
	}

	var body compactResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Compacted {
		t.Error("compaction must be skipped while writes are queued")
	}
	if body.SkipReason == "" {
		t.Error("a skip must say why, or the operator cannot act on it")
	}
}

func TestAdmin_CompactRejectsBadRequests(t *testing.T) {
	const proj = "proj-11"
	d := newTestDaemon(t, &fakeBackend{})
	srv := newAdminFixture(t, d, proj)

	// GET is not a mutation verb.
	resp, err := http.Get(srv.URL + "/v1/admin/compact-cache?project=" + proj)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}

	// No project named.
	resp, err = http.Post(srv.URL+"/v1/admin/compact-cache", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing-project status = %d, want 400", resp.StatusCode)
	}
}
