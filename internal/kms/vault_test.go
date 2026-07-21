package kms

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"testing"
	"time"
)

// vaultConfig returns the test Vault target. Overridable via env; defaults to
// the dev compose (host 8201, dev root token).
func vaultConfig() Config {
	addr := os.Getenv("SILO_TEST_VAULT_ADDR")
	if addr == "" {
		addr = "http://localhost:8201"
	}
	token := os.Getenv("SILO_TEST_VAULT_TOKEN")
	if token == "" {
		token = "dev-only-token"
	}
	// Isolate test data in a throwaway mount so it never touches real keys.
	return Config{Address: addr, Token: token, Mount: "silo-kms-test"}
}

// newLiveVault returns a KeyManager against a reachable Vault, or skips.
func newLiveVault(t *testing.T) *Vault {
	t.Helper()
	cfg := vaultConfig()
	u, _ := url.Parse(cfg.Address)
	conn, err := net.DialTimeout("tcp", u.Host, 500*time.Millisecond)
	if err != nil {
		t.Skipf("Vault not reachable at %s (%v) — skipping; run "+
			"`docker compose -f deploy/docker-compose.yaml up -d` to enable", cfg.Address, err)
	}
	_ = conn.Close()

	v, err := NewVault(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewVault: %v", err)
	}
	return v
}

func uniqueProject() string { return fmt.Sprintf("test-%d", time.Now().UnixNano()) }

func TestVault_CreateGetRevoke(t *testing.T) {
	v := newLiveVault(t)
	ctx := context.Background()
	proj := uniqueProject()

	keyID, err := v.CreateKey(ctx, proj)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if keyID == "" {
		t.Fatal("CreateKey: empty keyID")
	}
	t.Cleanup(func() { _ = v.RevokeKey(context.Background(), keyID) })

	// GetKey returns 32 bytes of usable SSE-C material.
	material, err := v.GetKey(ctx, keyID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if len(material) != keyBytes {
		t.Fatalf("GetKey: want %d-byte key, got %d", keyBytes, len(material))
	}

	// The key round-trips identically on a second read.
	again, err := v.GetKey(ctx, keyID)
	if err != nil {
		t.Fatalf("GetKey second: %v", err)
	}
	if !bytes.Equal(material, again) {
		t.Fatal("GetKey: key material differs between reads")
	}

	// Revoke, then GetKey -> ErrKeyNotFound.
	if err := v.RevokeKey(ctx, keyID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := v.GetKey(ctx, keyID); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("GetKey after revoke: want ErrKeyNotFound, got %v", err)
	}
	// Revoking again is a no-op (idempotent teardown).
	if err := v.RevokeKey(ctx, keyID); err != nil {
		t.Fatalf("RevokeKey idempotent: %v", err)
	}
}

func TestVault_KeysAreDistinctPerProject(t *testing.T) {
	v := newLiveVault(t)
	ctx := context.Background()
	pa, pb := uniqueProject(), uniqueProject()

	ka, err := v.CreateKey(ctx, pa)
	if err != nil {
		t.Fatalf("CreateKey A: %v", err)
	}
	t.Cleanup(func() { _ = v.RevokeKey(context.Background(), ka) })
	kb, err := v.CreateKey(ctx, pb)
	if err != nil {
		t.Fatalf("CreateKey B: %v", err)
	}
	t.Cleanup(func() { _ = v.RevokeKey(context.Background(), kb) })

	ma, _ := v.GetKey(ctx, ka)
	mb, _ := v.GetKey(ctx, kb)
	if bytes.Equal(ma, mb) {
		t.Fatal("two projects got identical key material — isolation broken")
	}
}

func TestVault_CreateKeyRefusesOverwrite(t *testing.T) {
	v := newLiveVault(t)
	ctx := context.Background()
	proj := uniqueProject()

	keyID, err := v.CreateKey(ctx, proj)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	t.Cleanup(func() { _ = v.RevokeKey(context.Background(), keyID) })

	// A second CreateKey for the same project must not silently rotate the key.
	if _, err := v.CreateKey(ctx, proj); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("duplicate CreateKey: want ErrKeyExists, got %v", err)
	}
}

func TestVault_GetMissingKey(t *testing.T) {
	v := newLiveVault(t)
	if _, err := v.GetKey(context.Background(), "projects/does-not-exist"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("GetKey missing: want ErrKeyNotFound, got %v", err)
	}
}
