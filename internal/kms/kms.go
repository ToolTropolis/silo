// Package kms issues and retrieves per-project encryption keys. The Vault
// implementation (vault.go) is the default; the interface allows swapping in a
// cloud KMS later without touching callers.
package kms

import (
	"context"
	"errors"
)

// KeyManager issues and retrieves per-project encryption keys.
type KeyManager interface {
	CreateKey(ctx context.Context, projectID string) (keyID string, err error)
	GetKey(ctx context.Context, keyID string) ([]byte, error) // raw key material, for SSE-C use
	RevokeKey(ctx context.Context, keyID string) error        // teardown step
}

// ErrKeyNotFound is returned by GetKey when no key exists for the keyID.
var ErrKeyNotFound = errors.New("kms: key not found")

// ErrKeyExists is returned by CreateKey when the project already has a key —
// so a re-run of onboarding never silently rotates a live key.
var ErrKeyExists = errors.New("kms: key already exists")
