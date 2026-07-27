package registry

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// uniqueSettingsID returns a run-unique key and removes its row afterwards.
func uniqueSettingsID(t *testing.T, r *Rqlite) string {
	t.Helper()
	id := fmt.Sprintf("set-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = r.DeleteSettings(context.Background(), id) })
	return id
}

func TestSettings_RoundTrip(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	id := uniqueSettingsID(t, r)

	want := CacheSettings{
		TTL:        ttl(90 * time.Minute),
		MaxEntries: entries(250),
		MaxBytes:   bytes64(64 << 20),
		UpdatedBy:  "test",
	}
	if err := r.PutSettings(ctx, id, want); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}

	got, err := r.GetSettings(ctx, id)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if !eqDur(got.TTL, want.TTL) {
		t.Errorf("TTL = %v, want %v", showDur(got.TTL), showDur(want.TTL))
	}
	if !eqInt(got.MaxEntries, want.MaxEntries) {
		t.Errorf("MaxEntries = %v, want 250", showInt(got.MaxEntries))
	}
	if got.MaxBytes == nil || *got.MaxBytes != *want.MaxBytes {
		t.Errorf("MaxBytes = %v, want %d", got.MaxBytes, *want.MaxBytes)
	}
	if got.UpdatedBy != "test" {
		t.Errorf("UpdatedBy = %q, want %q", got.UpdatedBy, "test")
	}
	if got.UpdatedAt == "" {
		t.Error("UpdatedAt should be stamped when not supplied")
	}
}

// The property the nullable schema exists for: a stored zero must come back as
// a set zero, and an unset field must come back nil. If SQL NULL and 0 collapse
// into each other here, "cache nothing" and "inherit the default" become the
// same request.
func TestSettings_NullIsDistinctFromZero(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	id := uniqueSettingsID(t, r)

	// TTL explicitly zero; the size caps left unset.
	if err := r.PutSettings(ctx, id, CacheSettings{TTL: ttl(0)}); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}
	got, err := r.GetSettings(ctx, id)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.TTL == nil {
		t.Fatal("an explicitly-zero TTL came back unset — NULL and 0 are being conflated")
	}
	if *got.TTL != 0 {
		t.Errorf("TTL = %v, want 0", *got.TTL)
	}
	if got.MaxEntries != nil {
		t.Errorf("MaxEntries = %v, want unset", showInt(got.MaxEntries))
	}
	if got.MaxBytes != nil {
		t.Errorf("MaxBytes = %v, want unset", *got.MaxBytes)
	}
}

// A project with no row inherits; that is not an error condition.
func TestSettings_AbsentProjectInherits(t *testing.T) {
	r := newLiveRegistry(t)
	got, err := r.GetSettings(context.Background(), "never-configured-project")
	if err != nil {
		t.Fatalf("an unconfigured project should not be an error: %v", err)
	}
	if !got.IsEmpty() {
		t.Errorf("got %+v, want empty settings", got)
	}
}

// Writing a nil field must clear the override rather than leave the old value
// in place — otherwise a policy could never be un-set from the console.
func TestSettings_PutClearsOmittedFields(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	id := uniqueSettingsID(t, r)

	if err := r.PutSettings(ctx, id, CacheSettings{TTL: ttl(time.Hour), MaxEntries: entries(10)}); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}
	// Re-write with only the TTL set.
	if err := r.PutSettings(ctx, id, CacheSettings{TTL: ttl(time.Hour)}); err != nil {
		t.Fatalf("PutSettings (update): %v", err)
	}

	got, err := r.GetSettings(ctx, id)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.MaxEntries != nil {
		t.Errorf("MaxEntries = %v, want cleared back to unset", showInt(got.MaxEntries))
	}
	if !eqDur(got.TTL, ttl(time.Hour)) {
		t.Errorf("TTL = %v, want it preserved", showDur(got.TTL))
	}
}

func TestSettings_DeleteRestoresInheritance(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	id := uniqueSettingsID(t, r)

	if err := r.PutSettings(ctx, id, CacheSettings{MaxEntries: entries(5)}); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}
	if err := r.DeleteSettings(ctx, id); err != nil {
		t.Fatalf("DeleteSettings: %v", err)
	}
	got, err := r.GetSettings(ctx, id)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if !got.IsEmpty() {
		t.Errorf("got %+v, want empty after delete", got)
	}
	// Deleting again is a no-op, not an error.
	if err := r.DeleteSettings(ctx, id); err != nil {
		t.Errorf("deleting absent settings should be a no-op: %v", err)
	}
}

func TestSettings_ListIncludesFleetDefaults(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	id := uniqueSettingsID(t, r)

	if err := r.PutSettings(ctx, id, CacheSettings{MaxEntries: entries(7)}); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}

	all, err := r.ListSettings(ctx)
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	got, ok := all[id]
	if !ok {
		t.Fatalf("project %q missing from ListSettings", id)
	}
	if !eqInt(got.MaxEntries, entries(7)) {
		t.Errorf("MaxEntries = %v, want 7", showInt(got.MaxEntries))
	}
}

// The cap has to survive the round trip, and "unset" has to stay distinct from
// an explicit zero — zero means reject every write, which is a real policy an
// operator might set deliberately.
func TestSettings_MaxEntryBytesRoundTrips(t *testing.T) {
	ctx := context.Background()
	r := newLiveRegistry(t)
	proj := uniqueSettingsID(t, r)

	limit := int64(102400)
	if err := r.PutSettings(ctx, proj, CacheSettings{MaxEntryBytes: &limit}); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}

	got, err := r.GetSettings(ctx, proj)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.MaxEntryBytes == nil {
		t.Fatal("MaxEntryBytes came back unset")
	}
	if *got.MaxEntryBytes != limit {
		t.Errorf("MaxEntryBytes = %d, want %d", *got.MaxEntryBytes, limit)
	}
	// Setting only this field must not invent values for the others.
	if got.TTL != nil || got.MaxEntries != nil || got.MaxBytes != nil {
		t.Errorf("unset retention fields were populated: %+v", got)
	}

	// An explicit zero is a policy, not an absence.
	zero := int64(0)
	if err := r.PutSettings(ctx, proj, CacheSettings{MaxEntryBytes: &zero}); err != nil {
		t.Fatalf("PutSettings zero: %v", err)
	}
	got, err = r.GetSettings(ctx, proj)
	if err != nil {
		t.Fatalf("GetSettings after zero: %v", err)
	}
	if got.MaxEntryBytes == nil {
		t.Fatal("an explicit zero was stored as NULL — it would silently mean 'inherit'")
	}
	if *got.MaxEntryBytes != 0 {
		t.Errorf("MaxEntryBytes = %d, want 0", *got.MaxEntryBytes)
	}

	// And clearing it restores inheritance.
	if err := r.PutSettings(ctx, proj, CacheSettings{}); err != nil {
		t.Fatalf("PutSettings clear: %v", err)
	}
	got, err = r.GetSettings(ctx, proj)
	if err != nil {
		t.Fatalf("GetSettings after clear: %v", err)
	}
	if got.MaxEntryBytes != nil {
		t.Errorf("clearing left %d behind, want inheritance", *got.MaxEntryBytes)
	}
}
