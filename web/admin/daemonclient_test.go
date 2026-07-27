package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A 409 is the daemon refusing to delete unsynced writes — the gate working.
// It must arrive as an outcome the operator can act on, not an error that reads
// like a broken console.
func TestDaemonClient_PurgeConflictIsAnOutcomeNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"pending":3,"error":"3 write(s) still queued"}`))
	}))
	defer srv.Close()

	c := NewDaemonClient(srv.URL)
	res, err := c.PurgeCache(context.Background(), "proj-11")
	if err != nil {
		t.Fatalf("a refusal must not be an error: %v", err)
	}
	if res.Purged {
		t.Error("Purged should be false on a refusal")
	}
	if res.Pending != 3 {
		t.Errorf("Pending = %d, want 3", res.Pending)
	}
	if res.Reason == "" {
		t.Error("a refusal must carry its reason")
	}
}

// A genuine server failure IS an error — it must not be mistaken for a refusal,
// which would tell the operator to drain a queue that is not the problem.
func TestDaemonClient_ServerErrorIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"disk failure"}`))
	}))
	defer srv.Close()

	c := NewDaemonClient(srv.URL)
	if _, err := c.PurgeCache(context.Background(), "proj-11"); err == nil {
		t.Error("a 500 must surface as an error")
	}
}

func TestDaemonClient_PurgeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"project":"proj-11","purged":true}`))
	}))
	defer srv.Close()

	res, err := NewDaemonClient(srv.URL).PurgeCache(context.Background(), "proj-11")
	if err != nil {
		t.Fatalf("PurgeCache: %v", err)
	}
	if !res.Purged {
		t.Error("Purged should be true")
	}
}

func TestDaemonClient_CompactReportsSizes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"compacted":true,"reclaimed_bytes":16252928,` +
			`"bytes_before":16777216,"bytes_after":524288}`))
	}))
	defer srv.Close()

	res, err := NewDaemonClient(srv.URL).CompactCache(context.Background(), "proj-11")
	if err != nil {
		t.Fatalf("CompactCache: %v", err)
	}
	if !res.Compacted || res.Reclaimed != 16252928 {
		t.Errorf("got %+v, want the reclaimed size reported", res)
	}
}

func TestDaemonClient_CompactSkipIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"compacted":false,"skip_reason":"1 write(s) queued"}`))
	}))
	defer srv.Close()

	res, err := NewDaemonClient(srv.URL).CompactCache(context.Background(), "proj-11")
	if err != nil {
		t.Fatalf("a safe skip must not be an error: %v", err)
	}
	if res.Compacted {
		t.Error("Compacted should be false")
	}
	if res.SkipReason == "" {
		t.Error("a skip must say why")
	}
}

func TestDaemonClient_CacheStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"projects":[{"project":"proj-11","pending":2,` +
			`"entries":250,"bytes":16201,"file_bytes":262144}]}`))
	}))
	defer srv.Close()

	stats, err := NewDaemonClient(srv.URL).CacheStats(context.Background())
	if err != nil {
		t.Fatalf("CacheStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("got %d projects, want 1", len(stats))
	}
	s := stats[0]
	if s.Pending != 2 || s.Entries != 250 || s.FileBytes != 262144 {
		t.Errorf("got %+v, want the full stat decoded", s)
	}
	if s.Reclaimable() != 262144-16201 {
		t.Errorf("Reclaimable() = %d", s.Reclaimable())
	}
}

// An unreachable daemon must be an error, so the view can say "unknown" rather
// than rendering zeros.
func TestDaemonClient_UnreachableIsAnError(t *testing.T) {
	c := NewDaemonClient("127.0.0.1:1") // nothing listens here
	if _, err := c.CacheStats(context.Background()); err == nil {
		t.Error("an unreachable daemon must report an error, not empty stats")
	}
}

// The address form decides the transport. A path is a Unix socket; a host:port
// is TCP. Getting this wrong would silently talk to the wrong endpoint.
func TestNewDaemonClient_AddressForms(t *testing.T) {
	tests := []struct {
		addr     string
		wantBase string
	}{
		{"http://127.0.0.1:8500", "http://127.0.0.1:8500"},
		{"127.0.0.1:8500", "http://127.0.0.1:8500"},
		{"./data/silod-admin.sock", "http://silod"},
		{"/var/run/silo/admin.sock", "http://silod"},
	}
	for _, tc := range tests {
		if got := NewDaemonClient(tc.addr).base; got != tc.wantBase {
			t.Errorf("NewDaemonClient(%q).base = %q, want %q", tc.addr, got, tc.wantBase)
		}
	}
}
