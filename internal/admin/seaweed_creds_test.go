package admin

import (
	"context"
	"strings"
	"testing"
)

// memSecretStore is an in-memory SecretStore for unit tests.
type memSecretStore struct {
	m map[string]string
}

func newMemSecretStore() *memSecretStore { return &memSecretStore{m: map[string]string{}} }
func (s *memSecretStore) PutSecret(_ context.Context, id, secret string) error {
	s.m[id] = secret
	return nil
}
func (s *memSecretStore) DeleteSecret(_ context.Context, id string) error {
	delete(s.m, id)
	return nil
}

// captureIssuer wires the issuer to a runner that records the commands it's
// asked to run, so we can assert on the s3.configure invocation without weed.
func captureIssuer(secret SecretStore) (*SeaweedCredentialIssuer, *[]string) {
	var cmds []string
	iss := &SeaweedCredentialIssuer{
		run: func(_ context.Context, command string) ([]byte, error) {
			cmds = append(cmds, command)
			return []byte("ok"), nil
		},
		secret: secret,
	}
	return iss, &cmds
}

func TestSeaweedIssue_BuildsScopedConfigureCommand(t *testing.T) {
	store := newMemSecretStore()
	iss, cmds := captureIssuer(store)

	credID, err := iss.Issue(context.Background(), "proj-11", "silo-proj-11")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if credID != "silo-cred-proj-11" {
		t.Fatalf("credID: want silo-cred-proj-11, got %q", credID)
	}
	if len(*cmds) != 1 {
		t.Fatalf("want 1 command, got %d", len(*cmds))
	}
	cmd := (*cmds)[0]
	// The identity must be named by the credentialID and scoped to exactly the
	// one bucket, with only Read,Write — this scoping is the isolation boundary.
	for _, want := range []string{
		"s3.configure",
		"-user silo-cred-proj-11",
		"-buckets silo-proj-11",
		"-actions Read,Write",
		"-apply",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("configure command missing %q: %s", want, cmd)
		}
	}
	// The secret must be persisted under the credentialID, never returned.
	if _, ok := store.m[credID]; !ok {
		t.Fatal("secret not stored under credentialID")
	}
}

func TestSeaweedRevoke_DeletesIdentityAndSecret(t *testing.T) {
	store := newMemSecretStore()
	iss, cmds := captureIssuer(store)

	credID, err := iss.Issue(context.Background(), "proj-9", "silo-proj-9")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := iss.Revoke(context.Background(), credID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	last := (*cmds)[len(*cmds)-1]
	if !strings.Contains(last, "-user silo-cred-proj-9") || !strings.Contains(last, "-delete") {
		t.Fatalf("revoke didn't delete the identity: %s", last)
	}
	if _, ok := store.m[credID]; ok {
		t.Fatal("secret not dropped on revoke")
	}
}

// TestSeaweedIssuer_SatisfiesInterface guards the CredentialIssuer contract.
func TestSeaweedIssuer_SatisfiesInterface(t *testing.T) {
	var _ CredentialIssuer = (*SeaweedCredentialIssuer)(nil)
}
