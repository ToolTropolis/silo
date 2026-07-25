package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tooltropolis/silo/internal/registry"
)

func ttl(d time.Duration) *time.Duration { return &d }
func entries(n int) *int                 { return &n }
func bytes64(n int64) *int64             { return &n }

type fakeRegistry struct {
	records []registry.ProjectRecord
	err     error
}

func (f *fakeRegistry) List(context.Context) ([]registry.ProjectRecord, error) {
	return f.records, f.err
}

func (f *fakeRegistry) Get(_ context.Context, id string) (registry.ProjectRecord, error) {
	if f.err != nil {
		return registry.ProjectRecord{}, f.err
	}
	for _, r := range f.records {
		if r.ProjectID == id {
			return r, nil
		}
	}
	return registry.ProjectRecord{}, registry.ErrNotFound
}

type fakeSettings struct {
	mu   sync.Mutex
	rows map[string]registry.CacheSettings
	err  error
	// puts records every write in order, so a test can assert exactly what was
	// stored rather than only what reads back.
	puts    []storedSettings
	deleted []string
}

type storedSettings struct {
	project  string
	settings registry.CacheSettings
}

func newFakeSettings(rows map[string]registry.CacheSettings) *fakeSettings {
	if rows == nil {
		rows = map[string]registry.CacheSettings{}
	}
	return &fakeSettings{rows: rows}
}

func (f *fakeSettings) GetSettings(_ context.Context, id string) (registry.CacheSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return registry.CacheSettings{}, f.err
	}
	return f.rows[id], nil
}

func (f *fakeSettings) ListSettings(context.Context) (map[string]registry.CacheSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]registry.CacheSettings{}
	for k, v := range f.rows {
		out[k] = v
	}
	return out, nil
}

func (f *fakeSettings) PutSettings(_ context.Context, id string, s registry.CacheSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.rows[id] = s
	f.puts = append(f.puts, storedSettings{project: id, settings: s})
	return nil
}

func (f *fakeSettings) DeleteSettings(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.rows, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeSettings) lastPut() (storedSettings, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.puts) == 0 {
		return storedSettings{}, false
	}
	return f.puts[len(f.puts)-1], true
}

type fakeDaemon struct {
	stats     []ProjectCacheStat
	statsErr  error
	purge     PurgeOutcome
	purgeErr  error
	compact   CompactOutcome
	compctErr error

	purged    []string
	compacted []string
}

func (f *fakeDaemon) CacheStats(context.Context) ([]ProjectCacheStat, error) {
	return f.stats, f.statsErr
}

func (f *fakeDaemon) PurgeCache(_ context.Context, id string) (PurgeOutcome, error) {
	f.purged = append(f.purged, id)
	return f.purge, f.purgeErr
}

func (f *fakeDaemon) CompactCache(_ context.Context, id string) (CompactOutcome, error) {
	f.compacted = append(f.compacted, id)
	return f.compact, f.compctErr
}

type fakeProvisioner struct {
	onboardErr  error
	teardownErr error
	plan        []TeardownStep

	onboarded []string
	steps     []string
}

func (f *fakeProvisioner) Onboard(_ context.Context, id string) error {
	f.onboarded = append(f.onboarded, id)
	return f.onboardErr
}

func (f *fakeProvisioner) TeardownStep(_ context.Context, id, step string) (string, error) {
	f.steps = append(f.steps, id+":"+step)
	return "", f.teardownErr
}

func (f *fakeProvisioner) TeardownPlan(context.Context, string) ([]TeardownStep, error) {
	return f.plan, nil
}

// newFixture serves the console over httptest with the given dependencies.
func newFixture(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// noRedirectClient stops the client following 303s, so a test can inspect the
// redirect target — which is where every action reports its outcome.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// flashOf returns the decoded flash or error message from a redirect.
func flashOf(t *testing.T, resp *http.Response) (flash, errMsg string) {
	t.Helper()
	loc := resp.Header.Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("bad redirect %q: %v", loc, err)
	}
	q := u.Query()
	return q.Get("flash"), q.Get("err")
}

// postForm submits a form and returns the un-followed response.
func postForm(t *testing.T, ts *httptest.Server, path string, form url.Values) *http.Response {
	t.Helper()
	resp, err := noRedirectClient().Post(ts.URL+path, "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// activeProject is the common single-project registry used across tests.
func activeProject(id string) *fakeRegistry {
	return &fakeRegistry{records: []registry.ProjectRecord{
		{ProjectID: id, BucketName: "silo-" + id, Status: registry.StatusActive},
	}}
}
