// Package admin holds the onboarding and teardown logic shared by the CLI and
// daemon. Onboarding is one automated command; teardown is deliberately manual,
// one confirmed step per layer.
package admin

import (
	"context"
	"errors"
)

var errNotImplemented = errors.New("admin: not implemented")

// Onboard provisions a project end to end: registry record, per-project SSE key,
// versioned bucket, and a scoped credential — rolling back completed steps on
// any failure (a compensating-action stack, not a distributed transaction).
// Not yet implemented — build sequence step 2 (docs/architecture.md).
func Onboard(ctx context.Context, projectID string) error {
	_ = ctx
	_ = projectID
	return errNotImplemented
}
