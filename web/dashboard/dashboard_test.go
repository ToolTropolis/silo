package dashboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/distilator"
	"github.com/tooltropolis/silo/internal/registry"
)

// --- fakes -----------------------------------------------------------------

type fakeRegistry struct {
	records []registry.ProjectRecord
	err     error
}

func (f *fakeRegistry) List(context.Context) ([]registry.ProjectRecord, error) {
	return f.records, f.err
}
func (f *fakeRegistry) Get(_ context.Context, id string) (registry.ProjectRecord, error) {
	for _, r := range f.records {
		if r.ProjectID == id {
			return r, nil
		}
	}
	return registry.ProjectRecord{}, registry.ErrNotFound
}

type fakeMemory struct {
	paths    []string
	versions []backend.ObjectVersion
	content  map[string]string
}

func (f *fakeMemory) ListPaths(context.Context, string, string) ([]string, error) {
	return f.paths, nil
}
func (f *fakeMemory) ListVersions(context.Context, string, string) ([]backend.ObjectVersion, error) {
	return f.versions, nil
}
func (f *fakeMemory) Get(_ context.Context, _, path, _ string) ([]byte, backend.ObjectVersion, error) {
	v, ok := f.content[path]
	if !ok {
		return nil, backend.ObjectVersion{}, backend.ErrNotFound
	}
	return []byte(v), backend.ObjectVersion{VersionID: "v1", ETag: "etag1"}, nil
}

type fakeReviewer struct {
	runs      []string
	run       *distilator.Run
	promoted  []string // recorded approvals
	promoteFn func([]string) ([]string, error)
}

func (f *fakeReviewer) ListRuns(context.Context, string) ([]string, error) { return f.runs, nil }
func (f *fakeReviewer) LoadRun(context.Context, string, string) (*distilator.Run, error) {
	if f.run == nil {
		return nil, distilator.ErrRunNotFound
	}
	return f.run, nil
}
func (f *fakeReviewer) Promote(_ context.Context, _, _ string, approved []string) ([]string, error) {
	f.promoted = approved
	if f.promoteFn != nil {
		return f.promoteFn(approved)
	}
	return approved, nil
}

func newTestServer(t *testing.T, reg Registry, mem MemoryReader, rev ProposalReviewer) *httptest.Server {
	t.Helper()
	s, err := NewServer(reg, mem, rev)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(b)
}

// --- registry view ---------------------------------------------------------

// TestRegistryView_RendersReferencesNotSecrets is the spec §7.1 security
// requirement: the registry view shows credential and key REFERENCES, never
// the credential secret or key material.
func TestRegistryView_RendersReferencesNotSecrets(t *testing.T) {
	reg := &fakeRegistry{records: []registry.ProjectRecord{{
		ProjectID:    "proj-11",
		BucketName:   "silo-proj-11",
		CredentialID: "cred-ref-abc123",
		KeyID:        "projects/proj-11",
		CreatedAt:    "2026-07-23T00:00:00Z",
		Status:       registry.StatusActive,
	}}}
	srv := newTestServer(t, reg, nil, nil)

	code, body := get(t, srv, "/")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{"proj-11", "silo-proj-11", "active"} {
		if !strings.Contains(body, want) {
			t.Errorf("registry page missing %q", want)
		}
	}
	// The record carries only references, so nothing secret-shaped can appear.
	// Guard against a future change that starts rendering raw material.
	for _, forbidden := range []string{"SECRET", "sk-ant-", "BEGIN PRIVATE KEY", "AKIA"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("registry page leaked something secret-shaped: %q", forbidden)
		}
	}
}

func TestRegistryView_EmptyState(t *testing.T) {
	srv := newTestServer(t, &fakeRegistry{}, nil, nil)
	code, body := get(t, srv, "/")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "No projects registered") {
		t.Error("expected an empty-state message")
	}
}

func TestRegistryView_SurfacesTeardownStep(t *testing.T) {
	reg := &fakeRegistry{records: []registry.ProjectRecord{{
		ProjectID: "p", Status: registry.StatusDecommissioning,
	}}}
	srv := newTestServer(t, reg, nil, nil)
	_, body := get(t, srv, "/")
	if !strings.Contains(body, "siloctl teardown") {
		t.Error("a decommissioning project should surface the next teardown command")
	}
}

func TestRegistryView_ReportsMissingDependency(t *testing.T) {
	srv := newTestServer(t, nil, nil, nil)
	code, body := get(t, srv, "/")
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if !strings.Contains(body, "no registry configured") {
		t.Errorf("expected an actionable message, got: %s", body)
	}
}

// --- memory view -----------------------------------------------------------

// TestMemoryView_HidesInternalNamespaces: run output and session transcripts
// are not "memory" and must not appear in the memory browser.
func TestMemoryView_HidesInternalNamespaces(t *testing.T) {
	mem := &fakeMemory{
		paths: []string{
			"memory/notes.md",
			distilator.RunPath("run-1", distilator.ProposalFile),
			distilator.SessionPath("sess-1"),
		},
		content: map[string]string{"memory/notes.md": "hello"},
	}
	srv := newTestServer(t, &fakeRegistry{}, mem, nil)

	_, body := get(t, srv, "/memory?project=proj-11")
	if !strings.Contains(body, "memory/notes.md") {
		t.Error("memory path should be listed")
	}
	if strings.Contains(body, distilator.OutputPrefix) {
		t.Error("Distilator run output leaked into the memory browser")
	}
	if strings.Contains(body, distilator.SessionPrefix) {
		t.Error("session transcripts leaked into the memory browser")
	}
}

func TestMemoryView_ShowsVersionsAndContent(t *testing.T) {
	mem := &fakeMemory{
		paths:    []string{"memory/notes.md"},
		versions: []backend.ObjectVersion{{VersionID: "v2", ETag: "e2"}, {VersionID: "v1", ETag: "e1"}},
		content:  map[string]string{"memory/notes.md": "the stored content"},
	}
	srv := newTestServer(t, &fakeRegistry{}, mem, nil)

	_, body := get(t, srv, "/memory?project=proj-11&path=memory/notes.md")
	if !strings.Contains(body, "the stored content") {
		t.Error("version content should render")
	}
	if !strings.Contains(body, "v2") || !strings.Contains(body, "v1") {
		t.Error("version history should list both versions")
	}
}

// --- distilator review -----------------------------------------------------

func testRun() *distilator.Run {
	return &distilator.Run{
		RunID:     "run-1",
		ProjectID: "proj-11",
		Proposals: []distilator.ProposedChange{
			{Path: "memory/a.md", NewContent: []byte("new A"), Rationale: "because A", Evidence: []string{"s1"}, Prevalence: 0.67},
			{Path: "memory/b.md", NewContent: []byte("new B"), Rationale: "because B", Evidence: []string{"s2"}, Prevalence: 0.33},
		},
	}
}

func TestDistilationsView_ShowsProposalsWithDiff(t *testing.T) {
	mem := &fakeMemory{content: map[string]string{"memory/a.md": "old A"}}
	rev := &fakeReviewer{runs: []string{"run-1"}, run: testRun()}
	srv := newTestServer(t, &fakeRegistry{}, mem, rev)

	_, body := get(t, srv, "/distilations?project=proj-11&run=run-1")
	for _, want := range []string{"memory/a.md", "because A", "old A", "new A", "s1", "67%"} {
		if !strings.Contains(body, want) {
			t.Errorf("proposal view missing %q", want)
		}
	}
	// b.md has no current content — it should be flagged as new, not error.
	if !strings.Contains(body, "new file") {
		t.Error("a proposal for a non-existent path should be marked as new")
	}
}

// TestPromote_OnlyWritesApproved is the review gate: unchecked proposals are
// never written.
func TestPromote_OnlyWritesApproved(t *testing.T) {
	rev := &fakeReviewer{runs: []string{"run-1"}, run: testRun()}
	srv := newTestServer(t, &fakeRegistry{}, &fakeMemory{}, rev)

	form := url.Values{
		"project": {"proj-11"},
		"run":     {"run-1"},
		"approve": {"memory/a.md"}, // only one of the two
	}
	resp, err := srv.Client().PostForm(srv.URL+"/promote", form)
	if err != nil {
		t.Fatalf("POST /promote: %v", err)
	}
	defer resp.Body.Close()

	if len(rev.promoted) != 1 || rev.promoted[0] != "memory/a.md" {
		t.Fatalf("only the approved path should be promoted, got %v", rev.promoted)
	}
}

// TestPromote_NothingApprovedWritesNothing — rejection is simply not approving.
func TestPromote_NothingApprovedWritesNothing(t *testing.T) {
	rev := &fakeReviewer{runs: []string{"run-1"}, run: testRun()}
	srv := newTestServer(t, &fakeRegistry{}, &fakeMemory{}, rev)

	form := url.Values{"project": {"proj-11"}, "run": {"run-1"}} // no approve values
	resp, err := srv.Client().PostForm(srv.URL+"/promote", form)
	if err != nil {
		t.Fatalf("POST /promote: %v", err)
	}
	defer resp.Body.Close()

	if rev.promoted != nil {
		t.Fatalf("nothing was approved, so Promote should not write: %v", rev.promoted)
	}
}

// TestPromote_SurfacesPartialFailure — a partial promotion must be visible.
func TestPromote_SurfacesPartialFailure(t *testing.T) {
	rev := &fakeReviewer{
		runs: []string{"run-1"}, run: testRun(),
		promoteFn: func(approved []string) ([]string, error) {
			return approved[:1], errors.New("backend down")
		},
	}
	srv := newTestServer(t, &fakeRegistry{}, &fakeMemory{}, rev)

	form := url.Values{
		"project": {"proj-11"}, "run": {"run-1"},
		"approve": {"memory/a.md", "memory/b.md"},
	}
	resp, err := srv.Client().PostForm(srv.URL+"/promote", form)
	if err != nil {
		t.Fatalf("POST /promote: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "Partially promoted") {
		t.Errorf("a partial promotion must be reported, got: %s", body)
	}
}

func TestPromote_RejectsGET(t *testing.T) {
	srv := newTestServer(t, &fakeRegistry{}, &fakeMemory{}, &fakeReviewer{})
	code, _ := get(t, srv, "/promote")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /promote = %d, want 405", code)
	}
}

// TestNoTeardownSurface: teardown is CLI-only (spec §7.1). The dashboard must
// expose no route that could revoke or delete anything.
func TestNoTeardownSurface(t *testing.T) {
	srv := newTestServer(t, &fakeRegistry{}, &fakeMemory{}, &fakeReviewer{})
	for _, path := range []string{"/teardown", "/revoke", "/delete", "/decommission"} {
		code, _ := get(t, srv, path)
		if code != http.StatusNotFound {
			t.Errorf("%s should not exist on the dashboard (got %d)", path, code)
		}
	}
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t, nil, nil, nil)
	code, body := get(t, srv, "/healthz")
	if code != http.StatusOK || body != "ok" {
		t.Fatalf("healthz = %d %q", code, body)
	}
}
