package daemon

import (
	"context"
	"time"

	"github.com/tooltropolis/silo/internal/cache"
)

// DefaultEvictInterval paces the eviction sweep. Much slower than the sync
// worker's: an unsynced write is urgent, a cache entry a few minutes past its
// TTL is not, and each pass takes a write transaction that would otherwise
// contend with a drain.
const DefaultEvictInterval = 5 * time.Minute

// EvictWorker keeps each project's cached content within its policy.
//
// Separate from SyncWorker rather than another job on its tick. The sync
// worker's steady state is deliberately one read-only transaction per project,
// its backoff state is documented as single-goroutine, and the two want
// different cadences — folding eviction in would undo all three.
type EvictWorker struct {
	daemon   *Daemon
	projects func() []string
	policy   func(projectID string) cache.EvictPolicy
	interval time.Duration
	logf     func(format string, args ...any)
}

// NewEvictWorker builds the sweep. policy is consulted per pass rather than
// captured once, so a configuration change takes effect without a restart.
func NewEvictWorker(d *Daemon, projects func() []string, policy func(string) cache.EvictPolicy,
	interval time.Duration, logf func(string, ...any)) *EvictWorker {
	if interval <= 0 {
		interval = DefaultEvictInterval
	}
	if projects == nil {
		projects = func() []string { return nil }
	}
	if policy == nil {
		policy = func(string) cache.EvictPolicy { return cache.EvictPolicy{} }
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &EvictWorker{daemon: d, projects: projects, policy: policy, interval: interval, logf: logf}
}

// Run sweeps on a ticker until ctx is cancelled.
func (w *EvictWorker) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.EvictOnce(ctx)
		}
	}
}

// EvictOnce sweeps every project once and returns what it removed.
func (w *EvictWorker) EvictOnce(ctx context.Context) []cache.EvictResult {
	var results []cache.EvictResult
	for _, projectID := range w.projects() {
		if ctx.Err() != nil {
			return results
		}
		policy := w.policy(projectID)
		if policy.Unlimited() {
			continue // nothing configured; don't even open the file
		}
		res, err := w.daemon.EvictCache(ctx, projectID, policy)
		if err != nil {
			w.logf("silod: evict %s: %v", projectID, err)
			continue
		}
		if res.Evicted() > 0 {
			w.logf("silod: evict %s: removed %d entr(ies) (%d expired, %d over cap), %d remaining",
				projectID, res.Evicted(), res.EvictedTTL, res.EvictedSize, res.EntriesAfter)
		}
		results = append(results, res)
	}
	return results
}
