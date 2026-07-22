package admin

import (
	"context"
	"fmt"
)

// NoopCredentialIssuer is a placeholder CredentialIssuer for the dev stack,
// where SeaweedFS runs with anonymous/open access and no IAM is configured. It
// returns a deterministic reference and treats revoke as a no-op, so onboarding
// and its rollback are exercisable end-to-end locally.
//
// GAP (spec §4 step 4): real per-project scoped credentials require SeaweedFS
// IAM (bucket-scoped access keys). That wiring is not implemented here and must
// replace this issuer before the isolation guarantee holds in a real
// deployment — see the cross-project isolation test (NAV-78), which will fail
// against this no-op issuer by design until real IAM lands.
type NoopCredentialIssuer struct{}

var _ CredentialIssuer = NoopCredentialIssuer{}

func (NoopCredentialIssuer) Issue(ctx context.Context, projectID, bucket string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// A stable, obviously-fake reference so it's clear in the registry that no
	// real credential backs this project yet.
	return fmt.Sprintf("dev-noop-cred:%s", projectID), nil
}

func (NoopCredentialIssuer) Revoke(ctx context.Context, credentialID string) error {
	return ctx.Err()
}
