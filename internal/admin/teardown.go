package admin

import "context"

// TeardownStep names one confirmed layer of the manual teardown flow. Each is a
// separate, individually-confirmed siloctl invocation — teardown is never a
// single unconfirmed command.
type TeardownStep string

const (
	StepRevokeCredential TeardownStep = "revoke-credential"
	StepRevokeKey        TeardownStep = "revoke-key"
	StepDeleteBucket     TeardownStep = "delete-bucket" // irreversible
	StepDeregister       TeardownStep = "deregister"
)

// Teardown performs a single confirmed teardown step for a project. Not yet
// implemented — build sequence step 7 (docs/architecture.md), last because it is
// destructive and least urgent for a working v1.
func Teardown(ctx context.Context, projectID string, step TeardownStep) error {
	_ = ctx
	_ = projectID
	_ = step
	return errNotImplemented
}
