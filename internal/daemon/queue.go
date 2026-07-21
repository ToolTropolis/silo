package daemon

import "context"

// syncWorker drains the local write queue back to the durable backend once it
// recovers, replaying each PendingWrite through the normal CAS path. Not yet
// implemented — build sequence step 3 (docs/architecture.md).
func (d *Daemon) syncWorker(ctx context.Context, projectID string) error {
	_ = ctx
	_ = projectID
	return nil
}
