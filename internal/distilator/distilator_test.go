package distilator

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
)

// memStore is an in-memory Store recording every write, so tests can assert
// exactly what the orchestrator touched.
type memStore struct {
	data   map[string][]byte // "project\x00path" -> content
	writes []string          // paths written, in order
}

func newMemStore() *memStore { return &memStore{data: map[string][]byte{}} }

func (m *memStore) key(projectID, path string) string { return projectID + "\x00" + path }

func (m *memStore) seed(projectID, path, content string) {
	m.data[m.key(projectID, path)] = []byte(content)
}

func (m *memStore) List(_ context.Context, projectID, prefix string) ([]string, error) {
	want := projectID + "\x00"
	var out []string
	for k := range m.data {
		if !strings.HasPrefix(k, want) {
			continue
		}
		p := strings.TrimPrefix(k, want)
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *memStore) Read(_ context.Context, projectID, path string) ([]byte, error) {
	v, ok := m.data[m.key(projectID, path)]
	if !ok {
		return nil, errors.New("not found: " + path)
	}
	return append([]byte(nil), v...), nil
}

func (m *memStore) Write(_ context.Context, projectID, path string, content []byte, _, _ string) error {
	m.data[m.key(projectID, path)] = append([]byte(nil), content...)
	m.writes = append(m.writes, path)
	return nil
}

// fakeProvider returns canned proposals and records what it was given.
type fakeProvider struct {
	proposals []ProposedChange
	err       error

	sawStore       map[string][]byte
	sawTranscripts []Transcript
}

func (f *fakeProvider) ProposeChanges(_ context.Context, currentStore map[string][]byte, transcripts []Transcript, _ string) ([]ProposedChange, error) {
	f.sawStore = currentStore
	f.sawTranscripts = transcripts
	return f.proposals, f.err
}

// fakeTranscripts serves a fixed batch.
type fakeTranscripts struct {
	sessions []Transcript
	err      error
}

func (f *fakeTranscripts) Recent(context.Context, string, int) ([]Transcript, error) {
	return f.sessions, f.err
}

const proj = "proj-11"

func seededStore() *memStore {
	s := newMemStore()
	s.seed(proj, "memory/prefs.md", "user prefers tabs")
	s.seed(proj, "memory/stack.md", "go + seaweedfs")
	return s
}

func oneSession() *fakeTranscripts {
	return &fakeTranscripts{sessions: []Transcript{{SessionID: "s1", Messages: []byte("...")}}}
}

// TestRun_NeverModifiesLiveStore is the spec's core Distilator invariant (§6.4):
// a run writes ONLY to its own output path; the input store is untouched.
func TestRun_NeverModifiesLiveStore(t *testing.T) {
	ctx := context.Background()
	store := seededStore()
	provider := &fakeProvider{proposals: []ProposedChange{
		{Path: "memory/prefs.md", NewContent: []byte("user prefers tabs, width 4"), Rationale: "seen repeatedly", Evidence: []string{"s1"}, Prevalence: 0.8},
	}}

	o := NewOrchestrator(provider, store, oneSession())
	run, err := o.Run(ctx, proj, "run-1", 24, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(run.Proposals) != 1 {
		t.Fatalf("want 1 proposal, got %d", len(run.Proposals))
	}

	// The ONLY write must be into the run's output namespace.
	if len(store.writes) != 1 {
		t.Fatalf("expected exactly 1 write, got %d: %v", len(store.writes), store.writes)
	}
	if !isOutputPath(store.writes[0]) {
		t.Fatalf("run wrote outside the output namespace: %q", store.writes[0])
	}

	// The live store content is unchanged.
	got, _ := store.Read(ctx, proj, "memory/prefs.md")
	if string(got) != "user prefers tabs" {
		t.Fatalf("live store was modified by the run: %q", got)
	}
}

func TestRun_WritesProposalManifest(t *testing.T) {
	ctx := context.Background()
	store := seededStore()
	want := ProposedChange{Path: "memory/new.md", NewContent: []byte("x"), Rationale: "r", Evidence: []string{"s1"}, Prevalence: 0.5}
	o := NewOrchestrator(&fakeProvider{proposals: []ProposedChange{want}}, store, oneSession())

	if _, err := o.Run(ctx, proj, "run-7", 24, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	blob, err := store.Read(ctx, proj, RunPath("run-7", ProposalFile))
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	var run Run
	if err := json.Unmarshal(blob, &run); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if run.RunID != "run-7" || len(run.Proposals) != 1 || run.Proposals[0].Path != want.Path {
		t.Fatalf("manifest mismatch: %+v", run)
	}
}

// TestRun_ExcludesPriorOutputFromInput confirms a run never consolidates a
// previous run's proposals as if they were live memory.
func TestRun_ExcludesPriorOutputFromInput(t *testing.T) {
	store := seededStore()
	store.seed(proj, RunPath("run-old", ProposalFile), `{"run_id":"run-old"}`)

	provider := &fakeProvider{}
	o := NewOrchestrator(provider, store, oneSession())
	if _, err := o.Run(context.Background(), proj, "run-2", 24, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for path := range provider.sawStore {
		if isOutputPath(path) {
			t.Fatalf("prior run output leaked into the provider's input: %q", path)
		}
	}
	if len(provider.sawStore) != 2 {
		t.Fatalf("want 2 live paths, got %d: %v", len(provider.sawStore), provider.sawStore)
	}
}

// TestRun_ExcludesSessionTranscriptsFromInput: captured transcripts live in the
// same project store as memory. They are evidence, not memory — a run must not
// hand its own transcripts to the provider as live-store content (which would
// let them be consolidated back into memory verbatim).
func TestRun_ExcludesSessionTranscriptsFromInput(t *testing.T) {
	store := seededStore()
	store.seed(proj, SessionPath("s1"), `{"messages":"raw session content"}`)

	provider := &fakeProvider{}
	o := NewOrchestrator(provider, store, oneSession())
	if _, err := o.Run(context.Background(), proj, "run-9", 24, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for path := range provider.sawStore {
		if isReservedPath(path) {
			t.Fatalf("reserved namespace leaked into the live-store view: %q", path)
		}
	}
	if len(provider.sawStore) != 2 {
		t.Fatalf("want only the 2 live memory paths, got %d: %v", len(provider.sawStore), provider.sawStore)
	}
}

// TestRun_RejectsProposalIntoSessionNamespace guards the transcript namespace
// the same way as the output namespace.
func TestRun_RejectsProposalIntoSessionNamespace(t *testing.T) {
	store := seededStore()
	provider := &fakeProvider{proposals: []ProposedChange{
		{Path: SessionPath("s1"), NewContent: []byte("tampered")},
	}}
	o := NewOrchestrator(provider, store, oneSession())

	if _, err := o.Run(context.Background(), proj, "run-10", 24, ""); err == nil {
		t.Fatal("expected rejection of a proposal targeting the session namespace")
	}
}

// TestRun_RejectsProposalIntoOutputNamespace guards against a provider writing
// into the run namespace.
func TestRun_RejectsProposalIntoOutputNamespace(t *testing.T) {
	store := seededStore()
	provider := &fakeProvider{proposals: []ProposedChange{
		{Path: RunPath("run-evil", "x.md"), NewContent: []byte("nope")},
	}}
	o := NewOrchestrator(provider, store, oneSession())

	if _, err := o.Run(context.Background(), proj, "run-3", 24, ""); err == nil {
		t.Fatal("expected rejection of a proposal targeting the output namespace")
	}
}

// TestRun_NoTranscriptsIsCleanNoOp — nothing to consolidate is success, and
// must not write a manifest.
func TestRun_NoTranscriptsIsCleanNoOp(t *testing.T) {
	store := seededStore()
	o := NewOrchestrator(&fakeProvider{}, store, &fakeTranscripts{})

	run, err := o.Run(context.Background(), proj, "run-4", 24, "")
	if err != nil {
		t.Fatalf("empty run should succeed, got %v", err)
	}
	if len(run.Proposals) != 0 {
		t.Fatalf("want no proposals, got %d", len(run.Proposals))
	}
	if len(store.writes) != 0 {
		t.Fatalf("empty run should write nothing, wrote %v", store.writes)
	}
}

func TestRun_RequiresIDs(t *testing.T) {
	o := NewOrchestrator(&fakeProvider{}, newMemStore(), oneSession())
	if _, err := o.Run(context.Background(), "", "run-5", 24, ""); err == nil {
		t.Fatal("empty projectID should error")
	}
	if _, err := o.Run(context.Background(), proj, "", 24, ""); err == nil {
		t.Fatal("empty runID should error")
	}
}

// TestRun_DropsEmptyPathProposals: a provider can return a well-formed but
// empty proposal object. Persisting it would put a useless entry in the
// manifest that only fails later, at promote time, with a confusing
// "not proposed by run" error. Drop it, but keep the valid ones.
func TestRun_DropsEmptyPathProposals(t *testing.T) {
	store := seededStore()
	provider := &fakeProvider{proposals: []ProposedChange{
		{Path: "", NewContent: []byte("junk")},            // empty — drop
		{Path: "   ", NewContent: []byte("junk")},         // whitespace — drop
		{Path: "memory/real.md", NewContent: []byte("x")}, // valid — keep
	}}
	o := NewOrchestrator(provider, store, oneSession())

	run, err := o.Run(context.Background(), proj, "run-empty", 24, "")
	if err != nil {
		t.Fatalf("one malformed proposal should not fail the run: %v", err)
	}
	if len(run.Proposals) != 1 {
		t.Fatalf("want 1 kept proposal, got %d: %+v", len(run.Proposals), run.Proposals)
	}
	if run.Proposals[0].Path != "memory/real.md" {
		t.Fatalf("kept the wrong proposal: %q", run.Proposals[0].Path)
	}
}
