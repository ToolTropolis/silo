package distilator

import "context"

// Promote applies an approved ProposedChange through the daemon's normal
// SafeWrite path — so it gets the same ETag/versioning treatment as any other
// write — and tags it promoted_from:<run-id>. Rejection leaves the proposal in
// _distilations/<run-id>/ for audit.
//
// Not yet implemented — build sequence step 5 (docs/architecture.md).
func Promote(ctx context.Context, projectID, runID string, change ProposedChange) error {
	_ = ctx
	_ = projectID
	_ = runID
	_ = change
	return errNotImplemented
}
