package daemon

import (
	"context"
	"math/rand/v2"
	"time"
)

// Sync worker defaults. The interval bounds how long a write can sit on local
// disk after the backend recovers; the backoff ceiling stops a long outage from
// probing a dead backend every tick for hours.
const (
	DefaultSyncInterval = 30 * time.Second
	maxSyncBackoff      = 5 * time.Minute
)

// SyncResult reports one project's drain attempt.
type SyncResult struct {
	ProjectID string
	Drained   int   // writes replayed to the backend
	Remaining int   // still buffered after the attempt
	Err       error // nil when the drain completed
}

// SyncWorker replays each project's locally-buffered writes once the backend is
// reachable again.
//
// Without it, the offline queue is write-only: SafeWrite buffers a write when
// the backend is down and nothing ever sends it. The data sits on one machine's
// disk indefinitely while every surface reports the project as healthy.
//
// Projects are synced sequentially within a tick rather than concurrently. A
// fleet has tens of projects, not thousands, so the wall-clock saving is
// irrelevant next to having one lock-contention story and deterministic logs.
type SyncWorker struct {
	daemon   *Daemon
	projects []string
	interval time.Duration
	logf     func(format string, args ...any)

	// Per-project backoff, so one project's dead backend never stalls another's
	// drain. Only touched from Run/SyncOnce, which are documented as
	// single-goroutine.
	backoff map[string]*backoffState
}

type backoffState struct {
	failures    int
	nextAttempt time.Time
}

// NewSyncWorker builds a worker for a fixed set of projects.
//
// The project list is a snapshot — in silod it comes from --tokens, since a
// project with no token takes no writes and so can have no queue. When the
// registry is wired into silod this should become registry.List() instead, to
// cover a project whose token was rotated away mid-outage.
func NewSyncWorker(d *Daemon, projects []string, interval time.Duration, logf func(string, ...any)) *SyncWorker {
	if interval <= 0 {
		interval = DefaultSyncInterval
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &SyncWorker{
		daemon:   d,
		projects: projects,
		interval: interval,
		logf:     logf,
		backoff:  make(map[string]*backoffState, len(projects)),
	}
}

// Run drains on a ticker until ctx is cancelled. It blocks, so callers run it in
// its own goroutine.
func (w *SyncWorker) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.SyncOnce(ctx)
		}
	}
}

// SyncOnce makes one pass over every project and returns what happened.
//
// Exported so shutdown and the flush command drive the same code path the
// ticker does, rather than a parallel implementation that can drift.
func (w *SyncWorker) SyncOnce(ctx context.Context) []SyncResult {
	var results []SyncResult
	now := time.Now()

	for _, projectID := range w.projects {
		if ctx.Err() != nil {
			return results
		}
		if st, ok := w.backoff[projectID]; ok && now.Before(st.nextAttempt) {
			continue // still backing off from a recent failure
		}

		// Skip projects with nothing buffered. This is the common case, and it
		// keeps the steady-state cost of the worker to one read-only bbolt
		// transaction per project per tick, with no network traffic at all.
		depth, err := w.daemon.QueueDepth(ctx, projectID)
		if err != nil {
			w.logf("silod: sync %s: reading queue depth: %v", projectID, err)
			continue
		}
		if depth == 0 {
			delete(w.backoff, projectID) // nothing pending; forget past failures
			continue
		}

		err = w.daemon.SyncProject(ctx, projectID)
		remaining, depthErr := w.daemon.QueueDepth(ctx, projectID)
		if depthErr != nil {
			remaining = -1 // unknown; don't report a wrong number
		}

		res := SyncResult{
			ProjectID: projectID,
			Drained:   depth - remaining,
			Remaining: remaining,
			Err:       err,
		}
		if remaining < 0 {
			res.Drained = 0
		}
		results = append(results, res)

		if err != nil {
			w.noteFailure(projectID, now)
			// Log the underlying error, not just "unreachable": a 403 from a
			// bad credential and a network outage both stop the drain, and
			// they need completely different fixes.
			w.logf("silod: sync %s: %v (%d still queued, retrying in %s)",
				projectID, err, remaining, w.backoffDelay(projectID))
			continue
		}

		delete(w.backoff, projectID)
		w.logf("silod: sync %s: drained %d write(s)", projectID, res.Drained)
	}
	return results
}

// noteFailure advances a project's backoff: interval * 2^failures, capped, with
// jitter so a fleet of projects recovering together doesn't resynchronise into a
// thundering herd against a backend that just came back.
func (w *SyncWorker) noteFailure(projectID string, now time.Time) {
	st, ok := w.backoff[projectID]
	if !ok {
		st = &backoffState{}
		w.backoff[projectID] = st
	}
	st.failures++
	st.nextAttempt = now.Add(w.delayFor(st.failures))
}

func (w *SyncWorker) delayFor(failures int) time.Duration {
	d := w.interval
	for i := 1; i < failures && d < maxSyncBackoff; i++ {
		d *= 2
	}
	if d > maxSyncBackoff {
		d = maxSyncBackoff
	}
	// ±20% jitter.
	jitter := 1 + (rand.Float64()*0.4 - 0.2)
	return time.Duration(float64(d) * jitter)
}

// backoffDelay reports the currently scheduled wait, for logging.
func (w *SyncWorker) backoffDelay(projectID string) time.Duration {
	st, ok := w.backoff[projectID]
	if !ok {
		return w.interval
	}
	return time.Until(st.nextAttempt).Round(time.Second)
}
