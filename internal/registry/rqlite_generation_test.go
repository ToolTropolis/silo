package registry

import (
	"context"
	"testing"
)

// A record written before 004 carries an empty generation, which the daemon
// treats as unverifiable — correct, but it leaves the project unable to cache
// anything. backfillGenerations fills those in.
func TestBackfillGenerations_FillsEmptyAndPreservesExisting(t *testing.T) {
	ctx := context.Background()
	r := newLiveRegistry(t)

	// A project as 004 would have left it: registered, no generation.
	legacy := uniqueRecord(t, r)
	legacy.Generation = ""
	if err := r.Register(ctx, legacy); err != nil {
		t.Fatalf("Register legacy: %v", err)
	}

	// And one that already has a generation, which must not be touched.
	current := uniqueRecord(t, r)
	current.Generation = "1111111111111111aaaaaaaaaaaaaaaa"
	if err := r.Register(ctx, current); err != nil {
		t.Fatalf("Register current: %v", err)
	}

	if err := r.backfillGenerations(ctx); err != nil {
		t.Fatalf("backfillGenerations: %v", err)
	}

	got, err := r.Get(ctx, legacy.ProjectID)
	if err != nil {
		t.Fatalf("Get legacy: %v", err)
	}
	assertGenerationShape(t, got.Generation)

	// An existing generation must survive: rewriting it would invalidate a live
	// cache and throw away every entry a running project had warmed.
	stillCurrent, err := r.Get(ctx, current.ProjectID)
	if err != nil {
		t.Fatalf("Get current: %v", err)
	}
	if stillCurrent.Generation != current.Generation {
		t.Errorf("existing generation was rewritten: %q -> %q",
			current.Generation, stillCurrent.Generation)
	}
}

// Two projects backfilled in the same pass must not share a generation: a
// shared value would let one project's cache file satisfy another's bind.
//
// This is the property a SQL backfill cannot hold on rqlite — randomblob() is
// substituted once per statement, so every row receives the same value. It
// failed exactly that way before the backfill moved into Go.
func TestBackfillGenerations_MintsDistinctValues(t *testing.T) {
	ctx := context.Background()
	r := newLiveRegistry(t)

	a, b := uniqueRecord(t, r), uniqueRecord(t, r)
	a.Generation, b.Generation = "", ""
	if err := r.Register(ctx, a); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	if err := r.Register(ctx, b); err != nil {
		t.Fatalf("Register b: %v", err)
	}

	if err := r.backfillGenerations(ctx); err != nil {
		t.Fatalf("backfillGenerations: %v", err)
	}

	ga, err := r.Get(ctx, a.ProjectID)
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	gb, err := r.Get(ctx, b.ProjectID)
	if err != nil {
		t.Fatalf("Get b: %v", err)
	}
	assertGenerationShape(t, ga.Generation)
	assertGenerationShape(t, gb.Generation)
	if ga.Generation == gb.Generation {
		t.Errorf("both projects got generation %q — one project's cache would "+
			"satisfy the other's bind", ga.Generation)
	}
}

// Re-running must be a no-op. ensureSchema calls the backfill on every startup,
// so a second pass rewriting live generations would cold-cache every project on
// every daemon restart.
func TestBackfillGenerations_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	r := newLiveRegistry(t)

	rec := uniqueRecord(t, r)
	rec.Generation = ""
	if err := r.Register(ctx, rec); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := r.backfillGenerations(ctx); err != nil {
		t.Fatalf("first backfill: %v", err)
	}
	first, err := r.Get(ctx, rec.ProjectID)
	if err != nil {
		t.Fatalf("Get after first: %v", err)
	}

	if err := r.backfillGenerations(ctx); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	second, err := r.Get(ctx, rec.ProjectID)
	if err != nil {
		t.Fatalf("Get after second: %v", err)
	}

	if first.Generation != second.Generation {
		t.Errorf("re-running rewrote the generation: %q -> %q — every restart "+
			"would discard the project's warm cache",
			first.Generation, second.Generation)
	}
}

// NewGeneration is the single minter shared by onboarding and the backfill.
func TestNewGeneration_ShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		g, err := NewGeneration()
		if err != nil {
			t.Fatalf("NewGeneration: %v", err)
		}
		assertGenerationShape(t, g)
		if seen[g] {
			t.Fatalf("NewGeneration repeated %q", g)
		}
		seen[g] = true
	}
}

// assertGenerationShape checks 128 bits rendered as lowercase hex — the shape
// the cache's stamp comparison assumes.
func assertGenerationShape(t *testing.T, gen string) {
	t.Helper()
	if len(gen) != 32 {
		t.Errorf("generation = %q (len %d), want 32 hex chars", gen, len(gen))
		return
	}
	for _, c := range gen {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("generation %q is not lowercase hex", gen)
			return
		}
	}
}
