package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// TokenPrefix marks a Silo agent token. A recognizable prefix means a leaked
// token is identifiable at a glance — in a log, a paste, or a secret scanner's
// ruleset — rather than looking like arbitrary base64.
const TokenPrefix = "silo_pat_"

// AgentToken is a stored token's metadata. The token itself is never in here:
// only its hash is persisted, so this struct describes a credential it cannot
// reproduce.
type AgentToken struct {
	Hash       string
	ProjectID  string
	Label      string
	CreatedAt  string
	CreatedBy  string
	LastUsedAt string
	RevokedAt  string
}

// Revoked reports whether the token has been revoked.
func (t AgentToken) Revoked() bool { return t.RevokedAt != "" }

// Display returns a safe identifier for the UI — the hash prefix, which is
// enough to tell two tokens apart without being usable as a credential.
func (t AgentToken) Display() string {
	if len(t.Hash) < 12 {
		return t.Hash
	}
	return t.Hash[:12]
}

// TokenStore issues and verifies the bearer tokens that scope an agent to one
// project.
//
// Separate from TenantRegistry for the same reason SettingsStore is: that
// interface is the project -> bucket/credential/key mapping, and it is what
// onboarding and teardown depend on. Tokens are credentials with their own
// lifecycle — minted, used, revoked — and every implementation of
// TenantRegistry should not be forced to serve them.
type TokenStore interface {
	// MintToken creates a token for a project and returns the raw token ONCE.
	// It is never retrievable again: only a hash is stored.
	MintToken(ctx context.Context, projectID, label, createdBy string) (rawToken string, err error)
	// VerifyToken resolves a raw token to its project, or ErrNotFound when the
	// token is unknown or revoked.
	VerifyToken(ctx context.Context, rawToken string) (projectID string, err error)
	// ListTokens returns a project's tokens (metadata only, no secrets).
	ListTokens(ctx context.Context, projectID string) ([]AgentToken, error)
	// RevokeToken marks a token dead by its hash. Revoking an already-revoked
	// or absent token is a no-op, so revocation is idempotent.
	RevokeToken(ctx context.Context, hash string) error
	// RevokeProjectTokens revokes every token for a project — used by teardown,
	// so a decommissioned project's credentials cannot outlive it.
	RevokeProjectTokens(ctx context.Context, projectID string) (int, error)
	// TouchToken records that a token was used. Best-effort: a failure here
	// must never fail the request that triggered it.
	TouchToken(ctx context.Context, hash string) error
}

// NewRawToken mints a cryptographically random token.
//
// 256 bits of CSPRNG output, base64url-encoded. Long enough that guessing is
// not a threat model, and URL-safe so it survives being pasted into config
// files, environment variables, and shell commands without escaping.
func NewRawToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("registry: generate token: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashToken returns the storage form of a raw token.
//
// SHA-256, hex-encoded. Not a password hash: see 006_agent_tokens.sql for why a
// deliberate work factor would be wrong here.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// LooksLikeToken reports whether a string has the shape of a Silo token. Used
// to give a clearer error than "unauthorized" when something else was pasted.
func LooksLikeToken(s string) bool {
	return strings.HasPrefix(s, TokenPrefix)
}
