package main

import (
	"strings"
	"testing"
)

// TestOnboard_RequiresS3Credentials is a regression guard.
//
// Onboarding creates buckets, so it must authenticate as the S3 admin identity.
// Once isolation landed (per-project scoped credentials), anonymous S3 access is
// disabled cluster-wide — but siloctl still built its backend client with no
// credentials, so every `siloctl onboard` failed with an opaque 403 from deep in
// the call. This asserts the flags exist and that a missing credential fails
// fast with an actionable message instead.
func TestOnboard_RequiresS3Credentials(t *testing.T) {
	t.Setenv("SILO_S3_ACCESS_KEY", "")
	t.Setenv("SILO_S3_SECRET_KEY", "")

	err := runOnboard([]string{
		"--project=p1",
		"--vault-token=dev-only-token",
	})
	if err == nil {
		t.Fatal("onboard without S3 credentials should fail fast, not attempt an anonymous request")
	}
	msg := err.Error()
	for _, want := range []string{"S3 admin credentials required", "bootstrap-dev.sh"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q so the operator knows the fix; got: %s", want, msg)
		}
	}
}

// TestOnboard_AcceptsCredentialsFromEnv confirms the env-var fallback is wired,
// so the flags aren't the only way to supply credentials.
func TestOnboard_AcceptsCredentialsFromEnv(t *testing.T) {
	t.Setenv("SILO_S3_ACCESS_KEY", "AKIATEST")
	t.Setenv("SILO_S3_SECRET_KEY", "SECRETTEST")

	// Point at an unroutable registry so the call fails AFTER credential
	// validation — proving validation passed without needing a live stack.
	err := runOnboard([]string{
		"--project=p1",
		"--vault-token=dev-only-token",
		"--rqlite=http://127.0.0.1:1",
	})
	if err == nil {
		t.Fatal("expected a connection failure against an unroutable registry")
	}
	if strings.Contains(err.Error(), "S3 admin credentials required") {
		t.Fatalf("credentials from env should satisfy validation; got: %v", err)
	}
}
