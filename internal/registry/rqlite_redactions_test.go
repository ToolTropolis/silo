package registry

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func uniqueRedactionProject(t *testing.T, r *Rqlite) string {
	t.Helper()
	id := fmt.Sprintf("red-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = r.conn.WriteOneContext(context.Background(),
			`DELETE FROM redactions WHERE project_id = '`+id+`'`)
	})
	return id
}

// The audit row is the whole reason the content can be destroyed: it outlives
// the bytes it describes.
func TestRedactions_RecordAndList(t *testing.T) {
	ctx := context.Background()
	r := newLiveRegistry(t)
	proj := uniqueRedactionProject(t, r)

	red := Redaction{
		ProjectID:  proj,
		Path:       "memory/notes.md",
		VersionID:  "v-leaked",
		Reason:     "contained an AWS key",
		RedactedBy: "operator",
	}
	if err := r.RecordRedaction(ctx, red); err != nil {
		t.Fatalf("RecordRedaction: %v", err)
	}

	got, err := r.ListRedactions(ctx, proj, "")
	if err != nil {
		t.Fatalf("ListRedactions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d redactions, want 1", len(got))
	}
	if got[0].VersionID != "v-leaked" || got[0].Path != "memory/notes.md" {
		t.Errorf("row = %+v, want it to name the destroyed version", got[0])
	}
	if got[0].Reason != "contained an AWS key" || got[0].RedactedBy != "operator" {
		t.Errorf("row lost the reason or actor: %+v", got[0])
	}
	// A timestamp is filled in even when the caller omits one: an audit row
	// without a time is not much of an audit.
	if got[0].RedactedAt == "" {
		t.Error("redacted_at was not populated")
	}
}

// Re-recording the same version must not rewrite the original row. An audit
// entry a later caller can edit is not an audit trail.
func TestRedactions_FirstRecordWins(t *testing.T) {
	ctx := context.Background()
	r := newLiveRegistry(t)
	proj := uniqueRedactionProject(t, r)

	first := Redaction{
		ProjectID: proj, Path: "memory/x.md", VersionID: "v1",
		Reason: "the real reason", RedactedBy: "alice",
	}
	if err := r.RecordRedaction(ctx, first); err != nil {
		t.Fatalf("RecordRedaction: %v", err)
	}
	// A second attempt with different details must be ignored, not applied.
	if err := r.RecordRedaction(ctx, Redaction{
		ProjectID: proj, Path: "memory/x.md", VersionID: "v1",
		Reason: "a rewritten reason", RedactedBy: "mallory",
	}); err != nil {
		t.Fatalf("second RecordRedaction: %v", err)
	}

	got, _ := r.ListRedactions(ctx, proj, "")
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 — the same version was recorded twice", len(got))
	}
	if got[0].Reason != "the real reason" || got[0].RedactedBy != "alice" {
		t.Errorf("the original audit row was overwritten: %+v", got[0])
	}
}

// The path filter is what the memory browser uses to mark redacted versions
// while rendering one file's history.
func TestRedactions_FilterByPath(t *testing.T) {
	ctx := context.Background()
	r := newLiveRegistry(t)
	proj := uniqueRedactionProject(t, r)

	for _, red := range []Redaction{
		{ProjectID: proj, Path: "memory/a.md", VersionID: "v1"},
		{ProjectID: proj, Path: "memory/a.md", VersionID: "v2"},
		{ProjectID: proj, Path: "memory/b.md", VersionID: "v3"},
	} {
		if err := r.RecordRedaction(ctx, red); err != nil {
			t.Fatalf("RecordRedaction: %v", err)
		}
	}

	all, _ := r.ListRedactions(ctx, proj, "")
	if len(all) != 3 {
		t.Errorf("unfiltered = %d rows, want 3", len(all))
	}
	onlyA, _ := r.ListRedactions(ctx, proj, "memory/a.md")
	if len(onlyA) != 2 {
		t.Errorf("filtered to memory/a.md = %d rows, want 2", len(onlyA))
	}
	for _, red := range onlyA {
		if red.Path != "memory/a.md" {
			t.Errorf("filter leaked a row for %q", red.Path)
		}
	}
}

// Redactions are scoped per project like everything else: one project's audit
// must not be visible from another.
func TestRedactions_ScopedToOneProject(t *testing.T) {
	ctx := context.Background()
	r := newLiveRegistry(t)
	projA := uniqueRedactionProject(t, r)
	projB := uniqueRedactionProject(t, r)

	if err := r.RecordRedaction(ctx, Redaction{
		ProjectID: projA, Path: "memory/secret.md", VersionID: "v1",
	}); err != nil {
		t.Fatalf("RecordRedaction: %v", err)
	}

	inB, _ := r.ListRedactions(ctx, projB, "")
	if len(inB) != 0 {
		t.Errorf("ISOLATION: project B sees %d of project A's redactions", len(inB))
	}
}
