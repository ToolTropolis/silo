package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/tooltropolis/silo/internal/admin"
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

// --- teardown confirmation gate -------------------------------------------

// TestTeardown_RequiresStep is the spec §5 guarantee: teardown cannot run as a
// single command. There is no flag that performs all four layers, and omitting
// --step is an error rather than a default.
func TestTeardown_RequiresStep(t *testing.T) {
	t.Setenv("SILO_S3_ACCESS_KEY", "AK")
	t.Setenv("SILO_S3_SECRET_KEY", "SK")

	err := runTeardown([]string{"--project=p1", "--vault-token=t"})
	if err == nil {
		t.Fatal("teardown without --step must fail")
	}
	if !strings.Contains(err.Error(), "one confirmed layer at a time") {
		t.Errorf("error should explain the per-layer rule; got: %v", err)
	}
}

func TestTeardown_RejectsUnknownStep(t *testing.T) {
	t.Setenv("SILO_S3_ACCESS_KEY", "AK")
	t.Setenv("SILO_S3_SECRET_KEY", "SK")

	err := runTeardown([]string{"--project=p1", "--step=nuke-it-all", "--vault-token=t"})
	if !errors.Is(err, admin.ErrUnknownStep) {
		t.Fatalf("want ErrUnknownStep, got %v", err)
	}
}

// TestConfirmStep_ReversibleStepsWantYes covers the ordinary prompt.
func TestConfirmStep_ReversibleStepsWantYes(t *testing.T) {
	for _, step := range []admin.TeardownStep{
		admin.StepRevokeCredential, admin.StepRevokeKey, admin.StepDeregister,
	} {
		if err := confirmStep(strings.NewReader("y\n"), &strings.Builder{}, "p1", step, false); err != nil {
			t.Errorf("%q with 'y': %v", step, err)
		}
		if err := confirmStep(strings.NewReader("n\n"), &strings.Builder{}, "p1", step, false); err == nil {
			t.Errorf("%q with 'n' should abort", step)
		}
		// A bare Enter must abort — the prompt is [y/N].
		if err := confirmStep(strings.NewReader("\n"), &strings.Builder{}, "p1", step, false); err == nil {
			t.Errorf("%q with empty input should abort", step)
		}
	}
}

// TestConfirmStep_IrreversibleNeedsProjectID: a reflexive "y" must not be able
// to delete a bucket. The operator has to type the project ID.
func TestConfirmStep_IrreversibleNeedsProjectID(t *testing.T) {
	step := admin.StepDeleteBucket

	if err := confirmStep(strings.NewReader("y\n"), &strings.Builder{}, "proj-11", step, false); err == nil {
		t.Fatal("'y' must NOT confirm the irreversible step")
	}
	if err := confirmStep(strings.NewReader("proj-99\n"), &strings.Builder{}, "proj-11", step, false); err == nil {
		t.Fatal("a different project ID must not confirm")
	}
	if err := confirmStep(strings.NewReader("proj-11\n"), &strings.Builder{}, "proj-11", step, false); err != nil {
		t.Fatalf("typing the exact project ID should confirm: %v", err)
	}
}

// TestConfirmStep_WarnsBeforeIrreversible: the operator must be told what is
// about to be destroyed.
func TestConfirmStep_WarnsBeforeIrreversible(t *testing.T) {
	var out strings.Builder
	_ = confirmStep(strings.NewReader("proj-11\n"), &out, "proj-11", admin.StepDeleteBucket, false)
	text := out.String()
	for _, want := range []string{"IRREVERSIBLE", "cannot be undone", "proj-11"} {
		if !strings.Contains(text, want) {
			t.Errorf("irreversible prompt missing %q; got: %s", want, text)
		}
	}
}

// TestConfirmStep_YesFlagSkipsPromptOnly confirms --yes bypasses the prompt for
// scripted use but does not bypass the one-step-per-invocation rule (that is
// enforced by --step being required, covered above).
func TestConfirmStep_YesFlagSkipsPrompt(t *testing.T) {
	for _, step := range []admin.TeardownStep{admin.StepRevokeCredential, admin.StepDeleteBucket} {
		if err := confirmStep(strings.NewReader(""), &strings.Builder{}, "p1", step, true); err != nil {
			t.Errorf("--yes should skip the prompt for %q: %v", step, err)
		}
	}
}

func TestNextStep(t *testing.T) {
	cases := map[admin.TeardownStep]admin.TeardownStep{
		admin.StepRevokeCredential: admin.StepRevokeKey,
		admin.StepRevokeKey:        admin.StepDeleteBucket,
		admin.StepDeleteBucket:     admin.StepDeregister,
		admin.StepDeregister:       "", // last
	}
	for step, want := range cases {
		if got := nextStep(step); got != want {
			t.Errorf("nextStep(%q) = %q, want %q", step, got, want)
		}
	}
}
