package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrCacheUnverified is returned when the daemon cannot establish which
// generation of a project owns its local cache, so it refuses to serve from it.
var ErrCacheUnverified = errors.New("daemon: cache ownership unverified")

// generations memoizes the project -> generation lookup.
//
// A generation is minted once at onboarding and never changes for that
// incarnation of the project, so one successful lookup is good for the life of
// the process. Failures are not cached: a registry blip must not pin a project
// into the unverified state until a restart.
type generations struct {
	mu  sync.RWMutex
	ids map[string]string
}

func newGenerations() *generations {
	return &generations{ids: make(map[string]string)}
}

func (g *generations) get(projectID string) (string, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	id, ok := g.ids[projectID]
	return id, ok
}

func (g *generations) put(projectID, generation string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ids[projectID] = generation
}

// bindCache makes sure the local cache for a project has been checked against
// the generation that currently owns it.
//
// Returns ErrCacheUnverified when there is no registry, the project has no
// generation (a record predating them), or the registry cannot be reached. The
// caller decides what that means: writes proceed regardless, but reads must not
// fall back to a cache whose owner is unknown.
func (d *Daemon) bindCache(ctx context.Context, projectID string) error {
	if gen, ok := d.generations.get(projectID); ok {
		// Already resolved; BindProject is idempotent for a matching handle.
		return d.cache.BindProject(ctx, projectID, gen)
	}
	if d.registry == nil {
		return fmt.Errorf("%w: no registry configured", ErrCacheUnverified)
	}

	rec, err := d.registry.Get(ctx, projectID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCacheUnverified, err)
	}
	if rec.Generation == "" {
		// Registered before generations existed. Treat as unverifiable rather
		// than as a match — an empty generation would match every file.
		return fmt.Errorf("%w: project %q has no generation", ErrCacheUnverified, projectID)
	}

	if err := d.cache.BindProject(ctx, projectID, rec.Generation); err != nil {
		return err
	}
	d.generations.put(projectID, rec.Generation)
	return nil
}
