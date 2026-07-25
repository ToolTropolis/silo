package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/tooltropolis/silo/internal/registry"
)

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

// TeardownOrder is the required sequence (spec §5). Access is revoked before
// data is destroyed, and the registry record — the only remaining trace of what
// a project was — is removed last, so an interrupted teardown is still
// diagnosable.
var TeardownOrder = []TeardownStep{
	StepRevokeCredential,
	StepRevokeKey,
	StepDeleteBucket,
	StepDeregister,
}

// ErrUnknownStep is returned for a step name that isn't one of the four.
var ErrUnknownStep = errors.New("admin: unknown teardown step")

// ErrOutOfOrder is returned when a step is attempted before its predecessors.
// Teardown is ordered because the later steps destroy what the earlier ones
// revoke access to: deleting a bucket while a live credential still points at
// it leaves that credential dangling.
var ErrOutOfOrder = errors.New("admin: teardown step out of order")

// ErrIrreversible marks the destructive step, so a caller can require a
// stronger confirmation for it than for the others.
var ErrIrreversible = errors.New("admin: irreversible step")

// IsIrreversible reports whether a step destroys data that cannot be recovered.
func IsIrreversible(step TeardownStep) bool { return step == StepDeleteBucket }

// ParseStep validates a step name.
func ParseStep(s string) (TeardownStep, error) {
	for _, known := range TeardownOrder {
		if TeardownStep(s) == known {
			return known, nil
		}
	}
	return "", fmt.Errorf("%w: %q (want one of revoke-credential, revoke-key, delete-bucket, deregister)", ErrUnknownStep, s)
}

// Teardown performs ONE confirmed teardown step for a project.
//
// It is deliberately not an end-to-end operation: the caller invokes it once
// per layer, confirming each time. Ordering is enforced against the registry
// record's status, so steps can't be skipped or replayed out of sequence.
//
// Status transitions (spec §5): the record moves to "decommissioning" after the
// first step and "decommissioned" after the last, so an interrupted teardown is
// visible in the registry and the dashboard.
func (o *Onboarder) Teardown(ctx context.Context, projectID string, step TeardownStep) error {
	if projectID == "" {
		return fmt.Errorf("admin: empty projectID")
	}
	if _, err := ParseStep(string(step)); err != nil {
		return err
	}

	rec, err := o.Registry.Get(ctx, projectID)
	if err != nil {
		return fmt.Errorf("admin: teardown %q: load record: %w", projectID, err)
	}
	if err := checkOrder(rec, step); err != nil {
		return err
	}

	switch step {
	case StepRevokeCredential:
		if err := o.Creds.Revoke(ctx, rec.CredentialID); err != nil {
			return fmt.Errorf("admin: teardown %q: revoke credential: %w", projectID, err)
		}
		// Mark the project as decommissioning before anything is destroyed, so
		// an interrupted teardown is visible rather than looking healthy.
		if err := o.Registry.UpdateStatus(ctx, projectID, registry.StatusDecommissioning); err != nil {
			return fmt.Errorf("admin: teardown %q: mark decommissioning: %w", projectID, err)
		}
		// Clear the ref only after the revoke succeeded, so a failure leaves the
		// step pending rather than skipping it.
		if err := o.Registry.UpdateRefs(ctx, projectID, rec.KeyID, ""); err != nil {
			return fmt.Errorf("admin: teardown %q: clear credential ref: %w", projectID, err)
		}

	case StepRevokeKey:
		if err := o.KMS.RevokeKey(ctx, rec.KeyID); err != nil {
			return fmt.Errorf("admin: teardown %q: revoke key: %w", projectID, err)
		}
		if err := o.Registry.UpdateRefs(ctx, projectID, "", ""); err != nil {
			return fmt.Errorf("admin: teardown %q: clear key ref: %w", projectID, err)
		}

	case StepDeleteBucket:
		// Purge the local cache BEFORE destroying the bucket, not after.
		//
		// The daemon refuses to purge while writes are still queued, and that
		// refusal has to stop the teardown while it can still do some good.
		// Deleting the bucket first would leave those writes addressed to a
		// destination that no longer exists — unsyncable, and lost whenever the
		// cache is eventually cleared.
		//
		// Purging here rather than at deregister also closes the window where a
		// project's memory sits in plaintext on local disk with nothing upstream
		// left for it to be consistent with.
		if o.Cache != nil {
			if err := o.Cache.PurgeCache(ctx, projectID); err != nil {
				return fmt.Errorf("admin: teardown %q: purge cache: %w", projectID, err)
			}
		} else {
			fmt.Printf("  NOTE: no daemon configured, so %q's local cache was not purged.\n"+
				"        Its memory remains on the daemon host until that file is removed.\n", projectID)
		}
		if err := o.Backend.DeleteBucket(ctx, projectID); err != nil {
			return fmt.Errorf("admin: teardown %q: delete bucket: %w", projectID, err)
		}
		if err := o.Registry.ClearBucket(ctx, projectID); err != nil {
			return fmt.Errorf("admin: teardown %q: clear bucket ref: %w", projectID, err)
		}

	case StepDeregister:
		if err := o.Registry.UpdateStatus(ctx, projectID, registry.StatusDecommissioned); err != nil {
			return fmt.Errorf("admin: teardown %q: mark decommissioned: %w", projectID, err)
		}
		if err := o.Registry.Deregister(ctx, projectID); err != nil {
			return fmt.Errorf("admin: teardown %q: deregister: %w", projectID, err)
		}
	}
	return nil
}

// NextStep reports the teardown step a project is due for, or "" when it is
// fully decommissioned. A deregistered project is gone from the registry, so
// ErrNotFound also means complete.
//
// Callers use this to tell the operator what to run next based on real state
// rather than on which step was last invoked.
func NextStep(ctx context.Context, reg registry.TenantRegistry, projectID string) (TeardownStep, error) {
	rec, err := reg.Get(ctx, projectID)
	if errors.Is(err, registry.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("admin: next step %q: %w", projectID, err)
	}
	return nextStep(rec), nil
}

// nextStep returns the step a project is due for, or "" when teardown is
// complete.
//
// Progress is derived from the record's own refs rather than tracked
// separately: each step clears the ref it consumed, so a cleared ref *is* the
// evidence that step ran. Nothing can drift out of sync with reality, and an
// interrupted teardown resumes correctly because the remaining refs still
// describe exactly what is left to destroy.
//
// Status alone cannot do this. It has three values for four steps, so steps 2-4
// all sit in "decommissioning" and become indistinguishable — which previously
// let deregister run while the bucket was still live, orphaning it.
func nextStep(rec registry.ProjectRecord) TeardownStep {
	switch {
	case rec.Status == registry.StatusDecommissioned:
		return ""
	case rec.CredentialID != "":
		return StepRevokeCredential
	case rec.KeyID != "":
		return StepRevokeKey
	case rec.BucketName != "":
		return StepDeleteBucket
	default:
		return StepDeregister
	}
}

// checkOrder enforces the step sequence.
//
// Teardown is strictly ordered because later steps destroy what earlier ones
// revoke access to, and because deregister erases the record that names the
// bucket — running it early strands data that can no longer be found through
// this CLI at all. So a step that isn't the next one due is refused, whether it
// runs ahead of its predecessors or repeats work already done.
func checkOrder(rec registry.ProjectRecord, step TeardownStep) error {
	want := nextStep(rec)
	if want == "" {
		return fmt.Errorf("%w: project %q is already fully decommissioned",
			ErrOutOfOrder, rec.ProjectID)
	}
	if step == want {
		return nil
	}

	// Distinguish "already done" from "too early" — they need different fixes.
	for _, done := range TeardownOrder {
		if done == want {
			break // reached the pending step without matching; must be ahead
		}
		if done == step {
			return fmt.Errorf("%w: project %q has already completed %q; next is %q",
				ErrOutOfOrder, rec.ProjectID, step, want)
		}
	}
	return fmt.Errorf("%w: project %q is not ready for %q; next is %q",
		ErrOutOfOrder, rec.ProjectID, step, want)
}
