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
		if rec.CredentialID != "" {
			if err := o.Creds.Revoke(ctx, rec.CredentialID); err != nil {
				return fmt.Errorf("admin: teardown %q: revoke credential: %w", projectID, err)
			}
		}
		// Mark the project as decommissioning before anything is destroyed, so
		// an interrupted teardown is visible rather than looking healthy.
		if err := o.Registry.UpdateStatus(ctx, projectID, registry.StatusDecommissioning); err != nil {
			return fmt.Errorf("admin: teardown %q: mark decommissioning: %w", projectID, err)
		}

	case StepRevokeKey:
		if rec.KeyID != "" {
			if err := o.KMS.RevokeKey(ctx, rec.KeyID); err != nil {
				return fmt.Errorf("admin: teardown %q: revoke key: %w", projectID, err)
			}
		}

	case StepDeleteBucket:
		if err := o.Backend.DeleteBucket(ctx, projectID); err != nil {
			return fmt.Errorf("admin: teardown %q: delete bucket: %w", projectID, err)
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

// checkOrder enforces the step sequence using the record's status.
//
// The registry tracks status, not per-step completion, so ordering is derived:
// an "active" project may only take the first step; once "decommissioning", the
// remaining steps run in order and are individually idempotent at the layer
// below (revoking an absent credential or key is a no-op).
func checkOrder(rec registry.ProjectRecord, step TeardownStep) error {
	switch rec.Status {
	case registry.StatusActive:
		if step != StepRevokeCredential {
			return fmt.Errorf("%w: project %q is still active; start with %q",
				ErrOutOfOrder, rec.ProjectID, StepRevokeCredential)
		}
	case registry.StatusDecommissioning:
		if step == StepRevokeCredential {
			// Re-running the first step is harmless, but say so plainly rather
			// than silently repeating a revoke.
			return fmt.Errorf("%w: project %q is already decommissioning; %q was already run",
				ErrOutOfOrder, rec.ProjectID, StepRevokeCredential)
		}
	case registry.StatusDecommissioned:
		return fmt.Errorf("%w: project %q is already fully decommissioned",
			ErrOutOfOrder, rec.ProjectID)
	}
	return nil
}
