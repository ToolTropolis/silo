package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	adminpkg "github.com/tooltropolis/silo/internal/admin"
	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/registry"
)

// OnboarderProvisioner adapts internal/admin's Onboarder to the console.
//
// It adds no lifecycle logic of its own — ordering, the compensating-action
// rollback, and the refusal to run a step out of sequence all stay in
// internal/admin, so the console and siloctl cannot drift apart on what
// teardown means.
type OnboarderProvisioner struct {
	Onboarder *adminpkg.Onboarder
	Registry  registry.TenantRegistry
}

var _ Provisioner = (*OnboarderProvisioner)(nil)

// probe is anything that can report its own reachability.
type probe interface {
	Probe(ctx context.Context) error
}

// CredsProbe exposes the credential issuer's liveness check for the wizard's
// preflight step, or nil when the issuer cannot report one. Returned as an
// interface so a non-probing issuer degrades to "cannot verify" rather than a
// false pass.
func (p *OnboarderProvisioner) CredsProbe() CredentialProber {
	if pr, ok := p.Onboarder.Creds.(probe); ok {
		return probeFunc(pr.Probe)
	}
	return nil
}

// BackendProbe reports whether the durable backend answers.
//
// Uses ListPaths against a project that does not exist: it is read-only,
// creates nothing, and still exercises the full credential-and-endpoint path.
// A missing bucket is a successful round trip, so only a transport or auth
// failure is reported as unreachable.
func (p *OnboarderProvisioner) BackendProbe() BackendProber {
	be := p.Onboarder.Backend
	if be == nil {
		return nil
	}
	return probeFunc(func(ctx context.Context) error {
		_, err := be.ListPaths(ctx, "silo-preflight-probe", "")
		if err == nil || errors.Is(err, backend.ErrNotFound) {
			return nil
		}
		// A bucket that does not exist is the expected answer here, and adapters
		// report it in their own words rather than always as ErrNotFound.
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "nosuchbucket") || strings.Contains(msg, "not found") ||
			strings.Contains(msg, "404") {
			return nil
		}
		return err
	})
}

func (p *OnboarderProvisioner) Onboard(ctx context.Context, projectID string) error {
	return p.Onboarder.Onboard(ctx, projectID)
}

func (p *OnboarderProvisioner) TeardownStep(ctx context.Context, projectID, step string) (string, error) {
	parsed, err := adminpkg.ParseStep(step)
	if err != nil {
		return "", err
	}
	if err := p.Onboarder.Teardown(ctx, projectID, parsed); err != nil {
		return "", err
	}

	// Report what is left rather than just "done", so an operator working
	// through four layers always knows where they are.
	next, err := adminpkg.NextStep(ctx, p.Registry, projectID)
	if err != nil || next == "" {
		return fmt.Sprintf("%s: %s complete — teardown finished", projectID, step), nil
	}
	return fmt.Sprintf("%s: %s complete — next is %s", projectID, step, next), nil
}

// TeardownPlan reports the four layers and which remain.
//
// Progress is derived from the registry record's own refs, the same way
// internal/admin derives it: each step clears the ref it consumed, so a cleared
// ref is the evidence that step ran. Nothing here tracks state separately, so
// nothing can drift out of sync with reality.
func (p *OnboarderProvisioner) TeardownPlan(ctx context.Context, projectID string) ([]TeardownStep, error) {
	next, err := adminpkg.NextStep(ctx, p.Registry, projectID)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}

	descriptions := map[adminpkg.TeardownStep]string{
		adminpkg.StepRevokeCredential: "Revoke the project's scoped S3 credential",
		adminpkg.StepRevokeKey:        "Revoke the per-project SSE key",
		adminpkg.StepDeleteBucket:     "Delete the bucket and every version in it, and purge the local cache",
		adminpkg.StepDeregister:       "Remove the registry record",
	}

	// Everything before the pending step has already run; the pending step and
	// everything after it have not. An empty `next` means teardown is finished,
	// so every step is done.
	pendingSeen := false
	out := make([]TeardownStep, 0, len(adminpkg.TeardownOrder))
	for _, step := range adminpkg.TeardownOrder {
		if next != "" && step == next {
			pendingSeen = true
		}
		out = append(out, TeardownStep{
			Name:        string(step),
			Description: descriptions[step],
			Done:        !pendingSeen,
		})
	}
	return out, nil
}
