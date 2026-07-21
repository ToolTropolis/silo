package kms

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"

	vault "github.com/hashicorp/vault/api"
)

// keyBytes is the AES-256 SSE key length. SeaweedFS SSE-C expects a 32-byte key.
const keyBytes = 32

// defaultMount is the KV v2 mount where Silo stores per-project key material.
// Kept separate from Vault's default `secret/` so Silo's keys are namespaced.
const defaultMount = "silo-kms"

// dataField is the KV entry field holding the base64-encoded key material.
const dataField = "key"

// Config points the Vault KeyManager at a server.
type Config struct {
	Address string // e.g. http://localhost:8201
	Token   string // dev-only in local; from a secrets source in prod
	Mount   string // KV v2 mount path; defaults to defaultMount
}

// Vault is the default KeyManager, backed by HashiCorp Vault's KV v2 engine.
//
// GetKey must return raw key material for SSE-C, so KV (store + read back) is
// used rather than the transit engine, which deliberately never exposes keys.
// Silo generates the key locally with crypto/rand and stores it; Vault is the
// system of record and access-control boundary.
type Vault struct {
	client *vault.Client
	mount  string
}

var _ KeyManager = (*Vault)(nil)

// NewVault connects to Vault and ensures the KV v2 mount exists.
func NewVault(ctx context.Context, cfg Config) (*Vault, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("kms: vault address required")
	}
	vc := vault.DefaultConfig()
	vc.Address = cfg.Address
	client, err := vault.NewClient(vc)
	if err != nil {
		return nil, fmt.Errorf("kms: new vault client: %w", err)
	}
	client.SetToken(cfg.Token)

	mount := cfg.Mount
	if mount == "" {
		mount = defaultMount
	}
	v := &Vault{client: client, mount: mount}
	if err := v.ensureMount(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

// ensureMount enables the KV v2 secrets engine at the configured path if it
// isn't already mounted. Idempotent so onboarding can run repeatedly.
func (v *Vault) ensureMount(ctx context.Context) error {
	mounts, err := v.client.Sys().ListMountsWithContext(ctx)
	if err != nil {
		return fmt.Errorf("kms: list mounts: %w", err)
	}
	if _, ok := mounts[v.mount+"/"]; ok {
		return nil
	}
	err = v.client.Sys().MountWithContext(ctx, v.mount, &vault.MountInput{
		Type:    "kv",
		Options: map[string]string{"version": "2"},
	})
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "path is already in use") {
		return fmt.Errorf("kms: enable kv mount %q: %w", v.mount, err)
	}
	return nil
}

// keyPath is the logical KV path for a project's key. It doubles as the keyID
// returned by CreateKey, so callers store an opaque reference on the registry.
func keyPath(projectID string) string {
	return path.Join("projects", projectID)
}

// CreateKey generates a fresh 32-byte key, stores it in Vault, and returns its
// keyID (the KV path). Refuses to overwrite an existing key for the project so
// a re-run of onboarding never silently rotates a live key.
func (v *Vault) CreateKey(ctx context.Context, projectID string) (string, error) {
	if projectID == "" {
		return "", fmt.Errorf("kms: empty projectID")
	}
	id := keyPath(projectID)

	// Guard against clobbering an existing key.
	if _, err := v.client.KVv2(v.mount).Get(ctx, id); err == nil {
		return "", fmt.Errorf("kms: key for project %q already exists: %w", projectID, ErrKeyExists)
	} else if !errors.Is(err, vault.ErrSecretNotFound) {
		return "", fmt.Errorf("kms: check existing key: %w", err)
	}

	material := make([]byte, keyBytes)
	if _, err := rand.Read(material); err != nil {
		return "", fmt.Errorf("kms: generate key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(material)

	if _, err := v.client.KVv2(v.mount).Put(ctx, id, map[string]interface{}{
		dataField: encoded,
	}); err != nil {
		return "", fmt.Errorf("kms: store key for %q: %w", projectID, err)
	}
	return id, nil
}

// GetKey returns the raw key material for a keyID, for SSE-C use.
func (v *Vault) GetKey(ctx context.Context, keyID string) ([]byte, error) {
	secret, err := v.client.KVv2(v.mount).Get(ctx, keyID)
	if err != nil {
		if errors.Is(err, vault.ErrSecretNotFound) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("kms: get key %q: %w", keyID, err)
	}
	raw, ok := secret.Data[dataField].(string)
	if !ok {
		return nil, fmt.Errorf("kms: key %q missing %q field", keyID, dataField)
	}
	material, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("kms: decode key %q: %w", keyID, err)
	}
	return material, nil
}

// RevokeKey permanently destroys a project's key material (teardown step).
// Destroying an already-absent key is a no-op, so teardown is idempotent.
func (v *Vault) RevokeKey(ctx context.Context, keyID string) error {
	// DeleteMetadata removes all versions + metadata — a true destroy, not a
	// soft delete, which is what teardown wants.
	if err := v.client.KVv2(v.mount).DeleteMetadata(ctx, keyID); err != nil {
		return fmt.Errorf("kms: revoke key %q: %w", keyID, err)
	}
	return nil
}
