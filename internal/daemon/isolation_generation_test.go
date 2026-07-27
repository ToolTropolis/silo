package daemon

import (
	"context"
	"strings"
	"testing"
)

// TestRead_NeverServesAPreviousTenantsCache is the executable form of the
// hazard this work exists to close.
//
// Found on a live stack: project "gatetest" was fully torn down — bucket
// deleted, key revoked, deregistered — and its cache file still held its memory
// in plaintext. Because the file is named after the projectID, re-onboarding
// the same ID inherited it, and the read path's outage fallback would have
// served the old tenant's bytes to the new project.
func TestRead_NeverServesAPreviousTenantsCache(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	reg := newGenRegistry()
	c, cleanup := newCacheForTest(t)
	defer cleanup()

	const proj, path = "proj-11", "memory/g1.md"

	// Tenant A writes memory while the backend is down, so it lands in the cache.
	reg.gen = "generation-A"
	dA := New(be, c, reg, nil)
	be.setDown(true)
	if _, err := dA.SafeWrite(ctx, proj, path,
		func([]byte) []byte { return []byte("gate 1") }, "agent", "s1"); err != nil {
		t.Fatalf("tenant A write: %v", err)
	}
	// It really is cached — otherwise this test proves nothing.
	if _, err := c.Get(ctx, proj, path); err != nil {
		t.Fatalf("setup: tenant A's content should be cached: %v", err)
	}

	// The project is torn down and re-onboarded under the SAME id, so it gets a
	// new generation. The cache file survives, as it does on disk today.
	reg.gen = "generation-B"
	dB := New(be, c, reg, nil)

	// Force the fallback branch: backend erroring but not 404.
	be.setDown(true)

	got, err := dB.Read(ctx, proj, path)
	if err == nil {
		t.Fatalf("the new tenant read the previous tenant's memory: %q", got)
	}
	if strings.Contains(string(got), "gate 1") {
		t.Fatalf("cross-tenant leak: got %q", got)
	}
}

// The unverifiable case: with no registry the daemon must refuse to serve the
// cache rather than guess at ownership.
func TestRead_FailsClosedWhenOwnershipIsUnverifiable(t *testing.T) {
	ctx := context.Background()
	be := newMapBackend()
	c, cleanup := newCacheForTest(t)
	defer cleanup()

	const proj, path = "proj-11", "memory/x.md"

	// Warm the cache through a verified daemon.
	verified := New(be, c, newGenRegistry(), nil)
	if _, err := verified.SafeWrite(ctx, proj, path,
		func([]byte) []byte { return []byte("content") }, "agent", "s1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A daemon with no registry cannot establish ownership.
	unverified := New(be, c, nil, nil)
	be.setDown(true)

	if _, err := unverified.Read(ctx, proj, path); err == nil {
		t.Fatal("an unverifiable cache must not be served")
	} else if !strings.Contains(err.Error(), "unverified") {
		t.Errorf("error should name the unverified cache, got: %v", err)
	}

	// And the verified daemon still serves it — fail-closed must not mean
	// fail-always.
	if got, err := verified.Read(ctx, proj, path); err != nil {
		t.Errorf("a verified cache must still serve during an outage: %v", err)
	} else if string(got) != "content" {
		t.Errorf("got %q, want %q", got, "content")
	}
}
