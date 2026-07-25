package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tooltropolis/silo/internal/registry"
)

// fakeTokenStore counts lookups so caching behaviour is observable.
type fakeTokenStore struct {
	mu      sync.Mutex
	tokens  map[string]string // raw token -> projectID
	err     error
	lookups int
	touched []string
}

func newFakeTokenStore(tokens map[string]string) *fakeTokenStore {
	if tokens == nil {
		tokens = map[string]string{}
	}
	return &fakeTokenStore{tokens: tokens}
}

func (f *fakeTokenStore) VerifyToken(_ context.Context, raw string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.err != nil {
		return "", f.err
	}
	p, ok := f.tokens[raw]
	if !ok {
		return "", registry.ErrNotFound
	}
	return p, nil
}

func (f *fakeTokenStore) TouchToken(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, hash)
	return nil
}

func (f *fakeTokenStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

func (f *fakeTokenStore) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeTokenStore) remove(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tokens, token)
}

func TestRegistryVerifier_ResolvesAToken(t *testing.T) {
	store := newFakeTokenStore(map[string]string{"tok-a": "proj-a"})
	v := NewRegistryTokenVerifier(store, nil, time.Minute)

	got, err := v.ProjectFor("tok-a")
	if err != nil {
		t.Fatalf("ProjectFor: %v", err)
	}
	if got != "proj-a" {
		t.Errorf("resolved to %q, want proj-a", got)
	}
}

func TestRegistryVerifier_RejectsUnknownAndEmpty(t *testing.T) {
	v := NewRegistryTokenVerifier(newFakeTokenStore(nil), nil, time.Minute)

	for _, tok := range []string{"", "nope", "silo_pat_wrong"} {
		if _, err := v.ProjectFor(tok); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("ProjectFor(%q) = %v, want ErrUnauthorized", tok, err)
		}
	}
}

// This runs on every agent request against a Raft cluster, so an uncached
// lookup would put a consensus read in front of every memory read.
func TestRegistryVerifier_CachesHits(t *testing.T) {
	store := newFakeTokenStore(map[string]string{"tok-a": "proj-a"})
	v := NewRegistryTokenVerifier(store, nil, time.Minute)

	for range 20 {
		if _, err := v.ProjectFor("tok-a"); err != nil {
			t.Fatalf("ProjectFor: %v", err)
		}
	}
	if n := store.count(); n != 1 {
		t.Errorf("hit the registry %d times, want 1 within the cache TTL", n)
	}
}

// Without negative caching, an agent looping with a bad token would hammer the
// registry — an accidental denial of service.
func TestRegistryVerifier_CachesMisses(t *testing.T) {
	store := newFakeTokenStore(nil)
	v := NewRegistryTokenVerifier(store, nil, time.Minute)

	for range 20 {
		if _, err := v.ProjectFor("bad"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("want ErrUnauthorized, got %v", err)
		}
	}
	if n := store.count(); n != 1 {
		t.Errorf("hit the registry %d times for a bad token, want 1", n)
	}
}

// A registry blip is not an authorization decision. Caching it as a negative
// would lock every agent out for the whole TTL after a transient failure.
func TestRegistryVerifier_DoesNotCacheRegistryFailures(t *testing.T) {
	store := newFakeTokenStore(map[string]string{"tok-a": "proj-a"})
	store.setErr(errors.New("rqlite unreachable"))
	v := NewRegistryTokenVerifier(store, nil, time.Minute)

	if _, err := v.ProjectFor("tok-a"); err == nil {
		t.Fatal("a registry failure should surface as an error")
	}
	if errors.Is(err0(v, "tok-a"), ErrUnauthorized) {
		t.Error("a registry failure must not be reported as unauthorized")
	}

	// Once the registry recovers, the token must work immediately.
	store.setErr(nil)
	got, err := v.ProjectFor("tok-a")
	if err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if got != "proj-a" {
		t.Errorf("resolved to %q, want proj-a", got)
	}
}

// err0 re-runs a lookup and returns just the error, for readability above.
func err0(v *RegistryTokenVerifier, tok string) error {
	_, err := v.ProjectFor(tok)
	return err
}

// The cache TTL is the window in which a revoked token still works, so it must
// actually expire.
func TestRegistryVerifier_CacheExpires(t *testing.T) {
	store := newFakeTokenStore(map[string]string{"tok-a": "proj-a"})
	v := NewRegistryTokenVerifier(store, nil, time.Minute)

	now := time.Now()
	v.now = func() time.Time { return now }

	if _, err := v.ProjectFor("tok-a"); err != nil {
		t.Fatalf("ProjectFor: %v", err)
	}
	// Revoke at the store, then move past the TTL.
	store.remove("tok-a")
	now = now.Add(2 * time.Minute)

	if _, err := v.ProjectFor("tok-a"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a revoked token must stop working once the cache expires, got %v", err)
	}
}

// Invalidate makes a revocation take effect at once rather than after the TTL.
func TestRegistryVerifier_InvalidateIsImmediate(t *testing.T) {
	store := newFakeTokenStore(map[string]string{"tok-a": "proj-a"})
	v := NewRegistryTokenVerifier(store, nil, time.Hour)

	if _, err := v.ProjectFor("tok-a"); err != nil {
		t.Fatalf("ProjectFor: %v", err)
	}
	store.remove("tok-a")
	v.Invalidate("tok-a")

	if _, err := v.ProjectFor("tok-a"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Invalidate should drop the cached entry, got %v", err)
	}
}

// A daemon started with --tokens must keep working, and must not need a
// reachable registry to serve those tokens.
func TestRegistryVerifier_StaticTokensStillWork(t *testing.T) {
	store := newFakeTokenStore(nil)
	store.setErr(errors.New("registry down"))
	static := StaticTokenVerifier{"dev-token": "proj-dev"}
	v := NewRegistryTokenVerifier(store, static, time.Minute)

	got, err := v.ProjectFor("dev-token")
	if err != nil {
		t.Fatalf("a static token should resolve without the registry: %v", err)
	}
	if got != "proj-dev" {
		t.Errorf("resolved to %q, want proj-dev", got)
	}
	if store.count() != 0 {
		t.Error("a static token should not hit the registry at all")
	}
}

// Both sources coexist: static for dev, registry for minted tokens.
func TestRegistryVerifier_StaticAndRegistryCoexist(t *testing.T) {
	store := newFakeTokenStore(map[string]string{"minted": "proj-minted"})
	static := StaticTokenVerifier{"dev-token": "proj-dev"}
	v := NewRegistryTokenVerifier(store, static, time.Minute)

	if got, _ := v.ProjectFor("dev-token"); got != "proj-dev" {
		t.Errorf("static token resolved to %q", got)
	}
	if got, _ := v.ProjectFor("minted"); got != "proj-minted" {
		t.Errorf("minted token resolved to %q", got)
	}
}

// A token resolves to exactly one project — the authorization boundary.
func TestRegistryVerifier_TokensAreProjectScoped(t *testing.T) {
	store := newFakeTokenStore(map[string]string{
		"tok-a": "proj-a",
		"tok-b": "proj-b",
	})
	v := NewRegistryTokenVerifier(store, nil, time.Minute)

	a, _ := v.ProjectFor("tok-a")
	b, _ := v.ProjectFor("tok-b")
	if a != "proj-a" || b != "proj-b" {
		t.Fatalf("ISOLATION FAILURE: tok-a=%q tok-b=%q", a, b)
	}
	if a == b {
		t.Fatal("ISOLATION FAILURE: two tokens resolved to the same project")
	}
}

// A last-used timestamp is a convenience for spotting stale tokens; it must
// never slow down or fail a memory read.
func TestRegistryVerifier_TouchIsAsynchronous(t *testing.T) {
	store := newFakeTokenStore(map[string]string{"tok-a": "proj-a"})
	v := NewRegistryTokenVerifier(store, nil, time.Minute)

	if _, err := v.ProjectFor("tok-a"); err != nil {
		t.Fatalf("ProjectFor: %v", err)
	}
	// The call returns without waiting for the touch; give the worker a moment
	// and confirm it happened.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		n := len(store.touched)
		store.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("last-used was never recorded")
}

func TestRegistryVerifier_ConcurrentLookupsAreSafe(t *testing.T) {
	store := newFakeTokenStore(map[string]string{"tok-a": "proj-a"})
	v := NewRegistryTokenVerifier(store, nil, time.Millisecond)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_, _ = v.ProjectFor("tok-a")
				_, _ = v.ProjectFor("bad")
				v.Invalidate("tok-a")
			}
		}()
	}
	wg.Wait()
}

// A nil store with no static tokens must refuse everything rather than panic.
func TestRegistryVerifier_NoSourcesRefusesEverything(t *testing.T) {
	v := NewRegistryTokenVerifier(nil, nil, time.Minute)
	if _, err := v.ProjectFor("anything"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("got %v, want ErrUnauthorized", err)
	}
}
