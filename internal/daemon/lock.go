package daemon

import (
	"context"
	"fmt"
	"time"
)

// defaultLease is how long an acquired project lock is held before it must be
// renewed. A daemon that dies without releasing loses the lock after this.
const defaultLease = 30 * time.Second

// AcquireLeadership tries to become the leader for a project's write path.
// Returns true if this daemon now holds the lock. Only the leader should run
// the write path / sync worker for that project; a non-leader defers.
//
// Requires the daemon to have been constructed with a Locker and instance ID
// (see WithLock). Without one, leadership can't be coordinated and it errors
// rather than silently letting two daemons write.
func (d *Daemon) AcquireLeadership(ctx context.Context, projectID string) (bool, error) {
	if d.locker == nil || d.instanceID == "" {
		return false, fmt.Errorf("daemon: no locker configured; cannot coordinate leadership")
	}
	return d.locker.Acquire(ctx, projectID, d.instanceID, defaultLease)
}

// ReleaseLeadership drops this daemon's lock for a project (graceful shutdown).
func (d *Daemon) ReleaseLeadership(ctx context.Context, projectID string) error {
	if d.locker == nil || d.instanceID == "" {
		return nil
	}
	return d.locker.Release(ctx, projectID, d.instanceID)
}

// leaseRenewInterval is how often a held lock should be renewed to keep it well
// inside the lease window. Renew at a third of the lease so a couple of missed
// renewals don't drop leadership.
func leaseRenewInterval() time.Duration { return defaultLease / 3 }
