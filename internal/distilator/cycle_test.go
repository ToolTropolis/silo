package distilator

import (
	"context"
	"testing"

	"github.com/tooltropolis/silo/internal/cache"
	"github.com/tooltropolis/silo/internal/daemon"
	"github.com/tooltropolis/silo/internal/testsupport"
)

// TestFullCycle_EndToEnd is the v1 definition-of-done criterion for the
// Distilator: transcripts in → proposed changes with evidence → written to a
// SEPARATE output path → input store unchanged → human approval promotes the
// changes through SafeWrite.
//
// It runs against the real daemon (in-memory backend + real bbolt cache), so
// the whole path — including the CAS write on promotion — is exercised.
func TestFullCycle_EndToEnd(t *testing.T) {
	ctx := context.Background()
	const project = "proj-cycle"

	be := testsupport.NewMemBackend()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	d := daemon.New(be, c, nil, nil)
	store := NewDaemonStore(d)

	// --- Seed: live memory + a captured session transcript. ---
	const livePath = "memory/prefs.md"
	const original = "user prefers spaces"
	if err := store.Write(ctx, project, livePath, []byte(original), "seed", "s0"); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if err := store.Write(ctx, project, SessionPath("s1"),
		[]byte(`{"messages":"user corrected: actually I use tabs"}`), "seed", "s0"); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}

	// --- Run: a fake provider stands in for Claude so the cycle is
	// deterministic; the orchestration path is the real one. ---
	provider := &fakeProvider{proposals: []ProposedChange{{
		Path:       livePath,
		NewContent: []byte("user prefers tabs"),
		Rationale:  "user corrected this in session s1",
		Evidence:   []string{"s1"},
		Prevalence: 1.0,
	}}}
	orch := NewOrchestrator(provider, store, NewStoreTranscripts(store))

	run, err := orch.Run(ctx, project, "run-e2e", 24, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Transcripts reached the provider as evidence...
	if len(provider.sawTranscripts) != 1 || provider.sawTranscripts[0].SessionID != "s1" {
		t.Fatalf("provider did not receive the transcript: %+v", provider.sawTranscripts)
	}
	// ...but NOT as live-store content.
	if _, leaked := provider.sawStore[SessionPath("s1")]; leaked {
		t.Fatal("transcript leaked into the live-store view")
	}
	if string(provider.sawStore[livePath]) != original {
		t.Fatalf("live store passed to provider was wrong: %q", provider.sawStore[livePath])
	}

	// Proposal carries evidence.
	if len(run.Proposals) != 1 || len(run.Proposals[0].Evidence) != 1 {
		t.Fatalf("expected 1 proposal with evidence, got %+v", run.Proposals)
	}

	// --- Invariant: the input store is UNCHANGED by the run. ---
	got, err := store.Read(ctx, project, livePath)
	if err != nil {
		t.Fatalf("read live path after run: %v", err)
	}
	if string(got) != original {
		t.Fatalf("run modified the live store: %q (want %q)", got, original)
	}

	// The proposal manifest exists at the separate output path.
	if _, err := store.Read(ctx, project, RunPath("run-e2e", ProposalFile)); err != nil {
		t.Fatalf("proposals not written to the output path: %v", err)
	}

	// --- Promote: human approval applies the change via SafeWrite. ---
	promoted, err := NewReviewer(store, d).Promote(ctx, project, "run-e2e", []string{livePath})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(promoted) != 1 {
		t.Fatalf("expected 1 promotion, got %v", promoted)
	}

	// The live store now carries the approved content.
	after, err := store.Read(ctx, project, livePath)
	if err != nil {
		t.Fatalf("read after promote: %v", err)
	}
	if string(after) != "user prefers tabs" {
		t.Fatalf("promotion did not land: %q", after)
	}

	// The run's output survives promotion, for audit.
	if _, err := store.Read(ctx, project, RunPath("run-e2e", ProposalFile)); err != nil {
		t.Fatalf("run output should survive promotion: %v", err)
	}
}
