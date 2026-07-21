package kms

import "context"

// Vault is the default KeyManager, backed by HashiCorp Vault. Per-project SSE
// keys are created on onboarding and revoked on teardown.
//
// Not yet implemented — build sequence step 2 (docs/architecture.md).
type Vault struct {
	// Vault address, token/auth, and transit/kv mount config land here.
}

var _ KeyManager = (*Vault)(nil)

func (v *Vault) CreateKey(ctx context.Context, projectID string) (string, error) {
	return "", errNotImplemented
}

func (v *Vault) GetKey(ctx context.Context, keyID string) ([]byte, error) {
	return nil, errNotImplemented
}

func (v *Vault) RevokeKey(ctx context.Context, keyID string) error { return errNotImplemented }
