package distilator

import (
	"context"
	"errors"
	"testing"
)

// recordingWriter captures SafeWrite calls so tests can assert promotion went
// through the CAS path with the right actor/session tagging.
type recordingWriter struct {
	calls []safeWriteCall
	err   error
}

type safeWriteCall struct {
	path      string
	content   string
	actor     string
	sessionID string
}

func (w *recordingWriter) SafeWrite(_ context.Context, _, path string, edit func([]byte) []byte, actor, sessionID string) error {
	if w.err != nil {
		return w.err
	}
	w.calls = append(w.calls, safeWriteCall{
		path:      path,
		content:   string(edit(nil)),
		actor:     actor,
		sessionID: sessionID,
	})
	return nil
}

// stageRun performs a run so its manifest exists, and returns the pieces needed
// to review it.
func stageRun(t *testing.T, proposals []ProposedChange) (*memStore, *recordingWriter, *Reviewer) {
	t.Helper()
	store := seededStore()
	o := NewOrchestrator(&fakeProvider{proposals: proposals}, store, oneSession())
	if _, err := o.Run(context.Background(), proj, "run-1", 24, ""); err != nil {
		t.Fatalf("stage run: %v", err)
	}
	w := &recordingWriter{}
	return store, w, NewReviewer(store, w)
}

// TestPromote_GoesThroughSafeWrite is the spec's promotion contract (§6.6):
// approved changes land via SafeWrite, tagged with the originating run.
func TestPromote_GoesThroughSafeWrite(t *testing.T) {
	_, w, r := stageRun(t, []ProposedChange{
		{Path: "memory/prefs.md", NewContent: []byte("updated prefs")},
	})

	promoted, err := r.Promote(context.Background(), proj, "run-1", []string{"memory/prefs.md"})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(promoted) != 1 || promoted[0] != "memory/prefs.md" {
		t.Fatalf("promoted mismatch: %v", promoted)
	}
	if len(w.calls) != 1 {
		t.Fatalf("expected 1 SafeWrite, got %d", len(w.calls))
	}
	call := w.calls[0]
	if call.path != "memory/prefs.md" || call.content != "updated prefs" {
		t.Fatalf("SafeWrite got wrong payload: %+v", call)
	}
	if call.actor != "distilator" {
		t.Fatalf("actor should be distilator, got %q", call.actor)
	}
	if call.sessionID != "promoted_from:run-1" {
		t.Fatalf("promotion not tagged with its run: %q", call.sessionID)
	}
}

// TestPromote_OnlyApprovedProposals — a rejected proposal is simply not
// promoted; approval is the human's decision, never inferred.
func TestPromote_OnlyApprovedProposals(t *testing.T) {
	_, w, r := stageRun(t, []ProposedChange{
		{Path: "memory/prefs.md", NewContent: []byte("approved")},
		{Path: "memory/stack.md", NewContent: []byte("rejected")},
	})

	// Approve only the first.
	if _, err := r.Promote(context.Background(), proj, "run-1", []string{"memory/prefs.md"}); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(w.calls) != 1 {
		t.Fatalf("only the approved proposal should be written, got %d writes", len(w.calls))
	}
	if w.calls[0].path != "memory/prefs.md" {
		t.Fatalf("wrong proposal promoted: %q", w.calls[0].path)
	}
}

// TestPromote_RejectedOutputSurvivesForAudit (§6.7): rejecting doesn't delete
// the run's output.
func TestPromote_RejectedOutputSurvivesForAudit(t *testing.T) {
	store, _, r := stageRun(t, []ProposedChange{
		{Path: "memory/prefs.md", NewContent: []byte("x")},
	})

	// Approve nothing.
	if _, err := r.Promote(context.Background(), proj, "run-1", nil); err != nil {
		t.Fatalf("Promote with no approvals: %v", err)
	}
	if _, err := store.Read(context.Background(), proj, RunPath("run-1", ProposalFile)); err != nil {
		t.Fatalf("run output should survive rejection for audit: %v", err)
	}
}

func TestPromote_UnknownPathIsRejected(t *testing.T) {
	_, w, r := stageRun(t, []ProposedChange{
		{Path: "memory/prefs.md", NewContent: []byte("x")},
	})

	_, err := r.Promote(context.Background(), proj, "run-1", []string{"memory/not-proposed.md"})
	if !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("want ErrProposalNotFound, got %v", err)
	}
	if len(w.calls) != 0 {
		t.Fatalf("nothing should be written for an unknown path, got %d", len(w.calls))
	}
}

func TestPromote_ReportsPartialOnFailure(t *testing.T) {
	store := seededStore()
	o := NewOrchestrator(&fakeProvider{proposals: []ProposedChange{
		{Path: "memory/a.md", NewContent: []byte("a")},
		{Path: "memory/b.md", NewContent: []byte("b")},
	}}, store, oneSession())
	if _, err := o.Run(context.Background(), proj, "run-1", 24, ""); err != nil {
		t.Fatalf("stage: %v", err)
	}

	w := &recordingWriter{err: errors.New("backend down")}
	r := NewReviewer(store, w)

	promoted, err := r.Promote(context.Background(), proj, "run-1", []string{"memory/a.md", "memory/b.md"})
	if err == nil {
		t.Fatal("expected the write failure to surface")
	}
	if len(promoted) != 0 {
		t.Fatalf("nothing succeeded, so promoted should be empty: %v", promoted)
	}
}

func TestLoadRun_MissingRun(t *testing.T) {
	store := seededStore()
	r := NewReviewer(store, &recordingWriter{})
	if _, err := r.LoadRun(context.Background(), proj, "no-such-run"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("want ErrRunNotFound, got %v", err)
	}
}

func TestPromote_RequiresWriter(t *testing.T) {
	store := seededStore()
	r := NewReviewer(store, nil)
	if _, err := r.Promote(context.Background(), proj, "run-1", []string{"x"}); err == nil {
		t.Fatal("promotion without a SafeWriter should error")
	}
}
