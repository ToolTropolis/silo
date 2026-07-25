package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tooltropolis/silo/internal/registry"
)

// TokenResolver is the slice of registry.TokenStore the verifier needs.
type TokenResolver interface {
	VerifyToken(ctx context.Context, rawToken string) (string, error)
	TouchToken(ctx context.Context, hash string) error
}

// DefaultTokenCacheTTL bounds how long a verified token stays cached.
//
// Short on purpose: it is the window in which a revoked token still works. A
// minute makes revocation feel immediate to an operator while keeping the
// registry off the hot path of every request.
const DefaultTokenCacheTTL = time.Minute

// RegistryTokenVerifier resolves agent tokens against the registry.
//
// This is what the TokenVerifier interface was always meant to have: tokens
// issued at onboarding, revocable without restarting a daemon, rather than a
// static map baked in at startup by --tokens.
//
// Results are cached because this runs on every agent request and rqlite is a
// Raft cluster — an uncached lookup would put a consensus read in front of
// every memory read. Both hits and misses are cached: without negative caching,
// an agent looping with a bad token would hammer the registry, which is exactly
// the shape of an accidental denial of service.
type RegistryTokenVerifier struct {
	store TokenResolver
	ttl   time.Duration
	now   func() time.Time

	mu    sync.RWMutex
	cache map[string]tokenEntry

	// static is consulted before the registry, so a daemon started with
	// --tokens keeps working. Dev and tests depend on this.
	static StaticTokenVerifier

	// touch records last-used timestamps off the request path.
	touch chan string
}

type tokenEntry struct {
	projectID string
	ok        bool
	expires   time.Time
}

var _ TokenVerifier = (*RegistryTokenVerifier)(nil)

// NewRegistryTokenVerifier builds a verifier over a token store. static may be
// nil; when set, it is checked first so flag-configured tokens keep working
// alongside registry-issued ones.
func NewRegistryTokenVerifier(store TokenResolver, static StaticTokenVerifier, ttl time.Duration) *RegistryTokenVerifier {
	if ttl <= 0 {
		ttl = DefaultTokenCacheTTL
	}
	v := &RegistryTokenVerifier{
		store:  store,
		ttl:    ttl,
		now:    time.Now,
		cache:  map[string]tokenEntry{},
		static: static,
		touch:  make(chan string, 64),
	}
	go v.touchWorker()
	return v
}

// ProjectFor implements TokenVerifier.
func (v *RegistryTokenVerifier) ProjectFor(token string) (string, error) {
	if token == "" {
		return "", ErrUnauthorized
	}

	// Static tokens first: they are in-memory, and a daemon configured with
	// --tokens should not need a reachable registry to serve them.
	if v.static != nil {
		if projectID, err := v.static.ProjectFor(token); err == nil {
			return projectID, nil
		}
	}
	if v.store == nil {
		return "", ErrUnauthorized
	}

	if e, ok := v.lookup(token); ok {
		if !e.ok {
			return "", ErrUnauthorized
		}
		v.scheduleTouch(token)
		return e.projectID, nil
	}

	// Bounded: an unreachable registry must fail the request rather than hang
	// an agent indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	projectID, err := v.store.VerifyToken(ctx, token)
	if errors.Is(err, registry.ErrNotFound) {
		v.store_(token, tokenEntry{ok: false, expires: v.now().Add(v.ttl)})
		return "", ErrUnauthorized
	}
	if err != nil {
		// A registry failure is not an authorization decision. Do NOT cache it:
		// caching a transient error as a negative result would lock every agent
		// out for the TTL after a blip.
		return "", err
	}

	v.store_(token, tokenEntry{projectID: projectID, ok: true, expires: v.now().Add(v.ttl)})
	v.scheduleTouch(token)
	return projectID, nil
}

func (v *RegistryTokenVerifier) lookup(token string) (tokenEntry, bool) {
	v.mu.RLock()
	e, ok := v.cache[token]
	v.mu.RUnlock()
	if !ok || v.now().After(e.expires) {
		return tokenEntry{}, false
	}
	return e, true
}

// store_ is named with a trailing underscore to avoid shadowing the store field.
func (v *RegistryTokenVerifier) store_(token string, e tokenEntry) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// Bound the cache so a flood of bad tokens cannot grow it without limit.
	// Dropping everything is fine: the cost is one registry lookup per live
	// token afterwards, and it keeps the eviction logic trivial.
	if len(v.cache) > 4096 {
		v.cache = map[string]tokenEntry{}
	}
	v.cache[token] = e
}

// Invalidate drops a token from the cache, so a revocation takes effect at once
// on this daemon rather than after the TTL.
func (v *RegistryTokenVerifier) Invalidate(token string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.cache, token)
}

// scheduleTouch records last-used asynchronously. Dropped when the buffer is
// full: a last-used timestamp is a convenience for spotting stale tokens, and
// it must never slow down or fail a memory read.
func (v *RegistryTokenVerifier) scheduleTouch(token string) {
	select {
	case v.touch <- token:
	default:
	}
}

func (v *RegistryTokenVerifier) touchWorker() {
	// Coalesce: an agent making many calls should produce one write, not one
	// per request.
	//
	// Uses the wall clock rather than v.now: this goroutine outlives any single
	// caller, and reading a field that a test (or a future caller) may replace
	// would be a data race. Coalescing does not need a controllable clock —
	// it is a rate limit on a best-effort timestamp, not observable behaviour.
	seen := map[string]time.Time{}
	for token := range v.touch {
		if last, ok := seen[token]; ok && time.Since(last) < time.Minute {
			continue
		}
		seen[token] = time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = v.store.TouchToken(ctx, registry.HashToken(token))
		cancel()
	}
}
