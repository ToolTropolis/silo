package registry

import (
	"strings"
	"testing"
)

// A token must be unguessable and recognizable: enough entropy that brute force
// is not a threat model, and a prefix so a leaked one is identifiable on sight.
func TestNewRawToken(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok, err := NewRawToken()
		if err != nil {
			t.Fatalf("NewRawToken: %v", err)
		}
		if !strings.HasPrefix(tok, TokenPrefix) {
			t.Fatalf("token %q lacks the %q prefix", tok, TokenPrefix)
		}
		body := strings.TrimPrefix(tok, TokenPrefix)
		// 32 random bytes base64url-encodes to 43 characters.
		if len(body) < 40 {
			t.Errorf("token body is %d chars, want >= 40 (256 bits of entropy)", len(body))
		}
		if seen[tok] {
			t.Fatalf("NewRawToken returned a duplicate: %q", tok)
		}
		seen[tok] = true
	}
}

// The whole point of hashing: a stored value must not reveal the token.
func TestHashToken(t *testing.T) {
	raw := "silo_pat_example"
	h := HashToken(raw)

	if h == raw {
		t.Fatal("the hash equals the token; storing it would store the credential")
	}
	if strings.Contains(h, raw) || strings.Contains(h, "example") {
		t.Errorf("the hash %q leaks part of the token", h)
	}
	if len(h) != 64 {
		t.Errorf("hash is %d chars, want 64 (hex-encoded SHA-256)", len(h))
	}
	if HashToken(raw) != h {
		t.Error("hashing must be deterministic, or a valid token would stop verifying")
	}
	if HashToken(raw+"x") == h {
		t.Error("different tokens must hash differently")
	}
}

// A token is URL-safe so it survives config files, env vars, and shell commands
// without escaping — the places it actually gets pasted.
func TestNewRawToken_IsSafeToPaste(t *testing.T) {
	tok, err := NewRawToken()
	if err != nil {
		t.Fatalf("NewRawToken: %v", err)
	}
	for _, bad := range []string{"+", "/", "=", " ", "\n", "\"", "'", "$", "\\"} {
		if strings.Contains(tok, bad) {
			t.Errorf("token %q contains %q, which needs escaping somewhere it will be pasted", tok, bad)
		}
	}
}

func TestLooksLikeToken(t *testing.T) {
	tok, _ := NewRawToken()
	if !LooksLikeToken(tok) {
		t.Error("a minted token should be recognized")
	}
	for _, notToken := range []string{"", "hunter2", "Bearer abc", "silo_", "pat_x"} {
		if LooksLikeToken(notToken) {
			t.Errorf("%q should not be recognized as a token", notToken)
		}
	}
}

func TestAgentToken_Display(t *testing.T) {
	tok := AgentToken{Hash: "0123456789abcdef0123456789abcdef"}
	got := tok.Display()

	if len(got) != 12 {
		t.Errorf("Display() = %q, want a 12-char prefix", got)
	}
	if !strings.HasPrefix(tok.Hash, got) {
		t.Errorf("Display() = %q, want a prefix of the hash", got)
	}
	// Short hashes must not panic.
	if short := (AgentToken{Hash: "abc"}).Display(); short != "abc" {
		t.Errorf("Display() on a short hash = %q", short)
	}
}

func TestAgentToken_Revoked(t *testing.T) {
	if (AgentToken{}).Revoked() {
		t.Error("a token with no revoked_at is live")
	}
	if !(AgentToken{RevokedAt: "2026-07-25T12:00:00Z"}).Revoked() {
		t.Error("a token with revoked_at is revoked")
	}
}
