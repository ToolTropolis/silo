package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
)

// SecretStore persists the raw secret key for an issued credential, keyed by the
// credentialID. The registry only ever holds the credentialID as a reference —
// the secret key lives here. A small adapter over kms.Vault satisfies this.
type SecretStore interface {
	PutSecret(ctx context.Context, credentialID, secret string) error
	DeleteSecret(ctx context.Context, credentialID string) error
}

// shellRunner runs a `weed shell` command string and returns combined output.
// Injectable so command construction is unit-testable without a real binary.
type shellRunner func(ctx context.Context, command string) ([]byte, error)

// SeaweedCredentialIssuer provisions a per-project SeaweedFS S3 identity scoped
// to that project's bucket, via `weed shell`'s s3.configure — the interface
// SeaweedFS maintains across versions (the on-disk identity layout is not
// stable). The identity's actions are Read/Write on exactly one bucket, so a
// project's credential cannot reach another project's bucket; enforcement is
// verified by the cross-project isolation test.
//
// The SeaweedFS identity is named by the credentialID (not the projectID), so
// Revoke — which receives only the credentialID — can delete the identity
// directly, and the interface stays honest.
type SeaweedCredentialIssuer struct {
	run    shellRunner
	secret SecretStore
}

var _ CredentialIssuer = (*SeaweedCredentialIssuer)(nil)

// SeaweedConfig configures the issuer's connection to a SeaweedFS cluster.
type SeaweedConfig struct {
	WeedBinary string // path to the `weed` binary (default "weed" on PATH)
	Filer      string // filer host:port, e.g. localhost:8888
	Master     string // master host:port, e.g. localhost:9333
}

// NewSeaweedCredentialIssuer builds an issuer that shells out to `weed`.
// secretStore is where issued secret keys are persisted (never the registry);
// pass nil to skip secret persistence (e.g. in tests).
func NewSeaweedCredentialIssuer(cfg SeaweedConfig, secretStore SecretStore) *SeaweedCredentialIssuer {
	return &SeaweedCredentialIssuer{run: weedShellRunner(cfg), secret: secretStore}
}

// weedShellRunner builds the real runner that pipes a command into `weed shell`.
func weedShellRunner(cfg SeaweedConfig) shellRunner {
	bin := cfg.WeedBinary
	if bin == "" {
		bin = "weed"
	}
	return func(ctx context.Context, command string) ([]byte, error) {
		args := []string{"shell"}
		if cfg.Master != "" {
			args = append(args, "-master", cfg.Master)
		}
		if cfg.Filer != "" {
			args = append(args, "-filer", cfg.Filer)
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Stdin = strings.NewReader(command + "\n")
		return cmd.CombinedOutput()
	}
}

// credentialName derives the SeaweedFS identity name (and credentialID) for a
// project. Prefixed so Silo's identities are recognizable in the S3 config.
func credentialName(projectID string) string { return "silo-cred-" + projectID }

// Issue creates a bucket-scoped identity for the project and returns its
// credentialID (the identity name). The generated secret key is stored in the
// SecretStore, not returned through the registry.
func (s *SeaweedCredentialIssuer) Issue(ctx context.Context, projectID, bucket string) (string, error) {
	credID := credentialName(projectID)
	accessKey, err := randKey(20)
	if err != nil {
		return "", fmt.Errorf("admin: generate access key: %w", err)
	}
	secretKey, err := randKey(40)
	if err != nil {
		return "", fmt.Errorf("admin: generate secret key: %w", err)
	}

	cmd := fmt.Sprintf(
		"s3.configure -user %s -access_key %s -secret_key %s -buckets %s -actions Read,Write -apply",
		credID, accessKey, secretKey, bucket,
	)
	out, err := s.run(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("admin: s3.configure issue for %q: %w (output: %s)", projectID, err, bytes.TrimSpace(out))
	}

	// Persist access+secret before returning; on failure the caller's rollback
	// Revokes the identity we just created.
	if s.secret != nil {
		if err := s.secret.PutSecret(ctx, credID, accessKey+":"+secretKey); err != nil {
			return "", fmt.Errorf("admin: store secret for %q: %w", projectID, err)
		}
	}
	return credID, nil
}

// Revoke removes the SeaweedFS identity named by credentialID and drops the
// stored secret. Deleting an absent identity is a no-op, so teardown and
// onboarding-rollback are idempotent.
func (s *SeaweedCredentialIssuer) Revoke(ctx context.Context, credentialID string) error {
	cmd := fmt.Sprintf("s3.configure -user %s -delete -apply", credentialID)
	if out, err := s.run(ctx, cmd); err != nil {
		return fmt.Errorf("admin: s3.configure delete %q: %w (output: %s)", credentialID, err, bytes.TrimSpace(out))
	}
	if s.secret != nil {
		if err := s.secret.DeleteSecret(ctx, credentialID); err != nil {
			return fmt.Errorf("admin: delete stored secret %q: %w", credentialID, err)
		}
	}
	return nil
}

// Probe reports whether credentials can actually be issued, without creating
// anything: `s3.configure` with no -apply only lists the current identities.
//
// Worth checking before onboarding starts, because this is the layer most
// likely to fail and the most awkward when it does. `weed` may be missing from
// PATH, or — against a containerized cluster — unable to reach the addresses
// SeaweedFS advertises, in which case it *hangs* rather than returning an
// error. Callers should pass a ctx with a deadline for that reason.
func (s *SeaweedCredentialIssuer) Probe(ctx context.Context) error {
	out, err := s.run(ctx, "s3.configure")
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("admin: `weed shell` did not respond: %w "+
				"(a host-native weed cannot reach a container-internal address and will hang)", ctx.Err())
		}
		return fmt.Errorf("admin: `weed shell` failed: %w (output: %s)", err, bytes.TrimSpace(out))
	}
	return nil
}

// randKey returns n crypto-random bytes hex-encoded (a shell-clean token).
func randKey(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
