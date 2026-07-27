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
	"github.com/tooltropolis/silo/internal/project"
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
	// Cache removes a project's local cache during teardown. Optional: a nil
	// purger warns rather than failing, so teardown still works on a host with
	// no daemon running — but the operator is told the local copy remains.
	Cache CachePurger
	// Settings removes a project's stored cache policy at deregister. Optional:
	// a nil store means the row is simply left behind, which is untidy rather
	// than dangerous.
	Settings SettingsRemover
	// Tokens revokes a project's agent tokens when its access is revoked.
	// Optional, but unlike Settings a nil store here is a real gap: the tokens
	// would keep authorizing against a project that no longer exists, so
	// teardown says so loudly rather than passing over it.
	Tokens TokenRevoker
}

// SettingsRemover deletes a project's stored cache policy.
//
// Narrower than registry.SettingsStore on purpose: teardown only ever needs to
// remove a row, and depending on the read/write surface would let a future
// change here start reading policy it has no business consulting.
type SettingsRemover interface {
	DeleteSettings(ctx context.Context, projectID string) error
}

// TokenRevoker kills every agent token issued for a project.
//
// Narrow for the same reason as SettingsRemover, and more pointedly: teardown
// must be able to revoke credentials but has no business minting them.
type TokenRevoker interface {
	RevokeProjectTokens(ctx context.Context, projectID string) (int, error)
}

// CachePurger drops a project's local cache file.
//
// Teardown destroys the bucket, the key, and the registry record, but the cache
// lives on a daemon's disk and is only reachable through that daemon — siloctl
// must not open bbolt itself, since a second process contending for the lock
// would hang both.
type CachePurger interface {
	PurgeCache(ctx context.Context, projectID string) error
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
	return o.OnboardWithRepo(ctx, projectID, RepoInfo{})
}

// RepoInfo records which repository a project serves. Both fields optional.
type RepoInfo struct {
	URL  string
	Path string
}

// Empty reports whether there is nothing to record.
func (r RepoInfo) Empty() bool { return r.URL == "" && r.Path == "" }

// OnboardWithRepo provisions a project and notes the repository it belongs to.
//
// The repo is recorded after provisioning succeeds, not as part of it: it is
// informational, and failing to write a note must never roll back a bucket, a
// key, and a credential that were created correctly.
func (o *Onboarder) OnboardWithRepo(ctx context.Context, projectID string, repo RepoInfo) (err error) {
	if err := o.onboard(ctx, projectID); err != nil {
		return err
	}
	if repo.Empty() {
		return nil
	}
	setter, ok := o.Registry.(interface {
		SetRepo(ctx context.Context, projectID, repoURL, repoPath string) error
	})
	if !ok {
		return nil
	}
	if err := setter.SetRepo(ctx, projectID, repo.URL, repo.Path); err != nil {
		// Deliberately not fatal, and deliberately not silent: the project is
		// fully provisioned and usable; only the note failed.
		fmt.Printf("  NOTE: %q was provisioned, but its repository could not be recorded: %v\n",
			projectID, err)
	}
	return nil
}

func (o *Onboarder) onboard(ctx context.Context, projectID string) (err error) {
	// Onboarding is the authoritative gate: a project that gets past here will
	// have its ID baked into a bucket name and a cache filename for good.
	if err := project.ValidateID(projectID); err != nil {
		return fmt.Errorf("admin: %w", err)
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
	//
	// The generation is minted here and never changes for this incarnation of
	// the project. It is what lets the daemon tell a fresh project apart from a
	// re-onboarded one reusing the same ID, and so keeps a previous tenant's
	// cached memory from being served to the new one.
	generation, err := newGeneration()
	if err != nil {
		return fmt.Errorf("admin: onboard %q: %w", projectID, err)
	}
	rec := registry.ProjectRecord{
		ProjectID:  projectID,
		BucketName: bucket,
		Status:     registry.StatusActive,
		Generation: generation,
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

// newGeneration mints an opaque identifier for one incarnation of a project.
//
// Delegates to registry.NewGeneration so onboarding and the pre-004 backfill
// mint the same shape from one implementation — a divergence here would be
// invisible until a cache failed to bind.
func newGeneration() (string, error) {
	return registry.NewGeneration()
}
