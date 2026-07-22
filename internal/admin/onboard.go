// Package admin holds the onboarding and teardown logic shared by the CLI and
// daemon. Onboarding is one automated command; teardown is deliberately manual,
// one confirmed step per layer.
package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/kms"
	"github.com/tooltropolis/silo/internal/registry"
)

// CredentialIssuer issues (and revokes) a bucket-scoped credential for a
// project. It's a seam because the concrete mechanism is SeaweedFS-IAM
// specific: production wires real per-bucket IAM keys here, while the dev stack
// (anonymous SeaweedFS) can use a no-op issuer. Onboarding depends only on this
// interface, so the rollback logic is identical either way.
type CredentialIssuer interface {
	// Issue returns an opaque credential reference (credentialID) scoped to the
	// project's bucket. The raw secret is stored wherever the issuer keeps it
	// (a secrets store), never returned through the registry.
	Issue(ctx context.Context, projectID, bucket string) (credentialID string, err error)
	// Revoke removes a previously issued credential (compensating action +
	// teardown). Revoking an absent credential must be a no-op.
	Revoke(ctx context.Context, credentialID string) error
}

// Onboarder provisions and decommissions projects by composing the registry,
// KMS, durable backend, and credential issuer.
type Onboarder struct {
	Registry registry.TenantRegistry
	KMS      kms.KeyManager
	Backend  backend.DurableBackend
	Creds    CredentialIssuer
}

// bucketName derives a project's bucket. Kept consistent with the backend
// adapter's own namespacing (silo-<projectID>).
func bucketName(projectID string) string { return "silo-" + projectID }

// Onboard provisions a project end to end in the spec's order (§4):
// registry record → per-project SSE key → versioned bucket → scoped credential.
// It maintains a stack of compensating actions and, on any failure, unwinds the
// steps already completed in reverse — a compensating-action stack, not a
// distributed transaction. A clean rollback leaves no half-provisioned project;
// a rollback that itself partially fails is reported alongside the original
// error so an operator can finish teardown by hand.
func (o *Onboarder) Onboard(ctx context.Context, projectID string) (err error) {
	if projectID == "" {
		return fmt.Errorf("admin: empty projectID")
	}
	bucket := bucketName(projectID)

	// compensations are run in reverse (LIFO) if err is non-nil at return.
	var compensations []func() error
	defer func() {
		if err == nil {
			return
		}
		rbErr := runCompensations(compensations)
		if rbErr != nil {
			err = fmt.Errorf("%w; ROLLBACK INCOMPLETE: %v", err, rbErr)
		}
	}()

	// Step 1 — registry record (status active).
	rec := registry.ProjectRecord{
		ProjectID:  projectID,
		BucketName: bucket,
		Status:     registry.StatusActive,
	}
	if err = o.Registry.Register(ctx, rec); err != nil {
		return fmt.Errorf("admin: onboard %q: register: %w", projectID, err)
	}
	compensations = append(compensations, func() error {
		return o.Registry.Deregister(context.WithoutCancel(ctx), projectID)
	})

	// Step 2 — per-project SSE key; record the keyID.
	keyID, err := o.KMS.CreateKey(ctx, projectID)
	if err != nil {
		return fmt.Errorf("admin: onboard %q: create key: %w", projectID, err)
	}
	compensations = append(compensations, func() error {
		return o.KMS.RevokeKey(context.WithoutCancel(ctx), keyID)
	})
	rec.KeyID = keyID

	// Step 3 — versioned bucket (the backend applies the SSE key on writes).
	if err = o.Backend.CreateBucket(ctx, projectID); err != nil {
		return fmt.Errorf("admin: onboard %q: create bucket: %w", projectID, err)
	}
	compensations = append(compensations, func() error {
		return o.Backend.DeleteBucket(context.WithoutCancel(ctx), projectID)
	})

	// Step 4 — scoped credential; record the credentialID.
	credID, err := o.Creds.Issue(ctx, projectID, bucket)
	if err != nil {
		return fmt.Errorf("admin: onboard %q: issue credential: %w", projectID, err)
	}
	compensations = append(compensations, func() error {
		return o.Creds.Revoke(context.WithoutCancel(ctx), credID)
	})
	rec.CredentialID = credID

	// Persist the keyID + credentialID onto the record now that both exist.
	if err = o.Registry.UpdateRefs(ctx, projectID, keyID, credID); err != nil {
		return fmt.Errorf("admin: onboard %q: persist refs: %w", projectID, err)
	}

	return nil
}

// runCompensations executes compensating actions in reverse order, continuing
// past individual failures so every layer gets a teardown attempt. Returns the
// joined errors, or nil if all succeeded.
func runCompensations(comps []func() error) error {
	var errs []error
	for i := len(comps) - 1; i >= 0; i-- {
		if e := comps[i](); e != nil {
			errs = append(errs, e)
		}
	}
	return errors.Join(errs...)
}
