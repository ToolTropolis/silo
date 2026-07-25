package daemon

import (
	"context"
	"testing"

	"github.com/tooltropolis/silo/internal/cache"

	"github.com/tooltropolis/silo/internal/registry"
)

// genRegistry is a minimal TenantRegistry that reports a generation for every
// project, so tests exercise the normal verified path rather than the
// fail-closed one. Tests that want to exercise unverifiable ownership pass a
// nil registry or set gen to "".
type genRegistry struct {
	registry.TenantRegistry
	gen string
	err error
}

func newGenRegistry() *genRegistry { return &genRegistry{gen: "gen-test"} }

func (r *genRegistry) Get(_ context.Context, projectID string) (registry.ProjectRecord, error) {
	if r.err != nil {
		return registry.ProjectRecord{}, r.err
	}
	return registry.ProjectRecord{
		ProjectID:  projectID,
		BucketName: "silo-" + projectID,
		Status:     registry.StatusActive,
		Generation: r.gen,
	}, nil
}

// newCacheForTest returns a cache that outlives a single daemon, so a test can
// hand the same on-disk file to two daemons — which is exactly what happens
// when a project is torn down and re-onboarded under the same ID.
func newCacheForTest(t *testing.T) (*cache.BoltCache, func()) {
	t.Helper()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	return c, func() { _ = c.Close() }
}
