package admin

import (
	"context"
	"fmt"
)

// NoopCredentialIssuer is a stand-in CredentialIssuer that provisions no real
// credential — it returns a deterministic fake reference and treats revoke as a
// no-op. It exists so the onboarding orchestration (and its rollback) can be
// tested without a live SeaweedFS.
//
// It does NOT enforce isolation. Production and any isolation-sensitive path use
// SeaweedCredentialIssuer, which creates a real per-project bucket-scoped
// SeaweedFS identity (see the cross-project isolation test). siloctl wires the
// real issuer; this type is for tests only.
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
