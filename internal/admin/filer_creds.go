package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// SecretStore persists the raw secret key for an issued credential, keyed by the
// credentialID. The registry only ever holds the credentialID as a reference —
// the secret key lives here. A small adapter over kms.Vault satisfies this.
type SecretStore interface {
	PutSecret(ctx context.Context, credentialID, secret string) error
	DeleteSecret(ctx context.Context, credentialID string) error
}

// credentialName derives the SeaweedFS identity name (and credentialID) for a
// project. Prefixed so Silo's identities are recognizable in the S3 config.
func credentialName(projectID string) string { return "silo-cred-" + projectID }

// randKey returns n crypto-random bytes hex-encoded.
func randKey(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// identityDir is where SeaweedFS keeps S3 identities in the filer: one JSON
// file per identity, named after it.
const identityDir = "/etc/iam/identities"

// s3Identity is SeaweedFS's on-disk identity document.
//
// Verified against a live cluster rather than taken from docs: an identity
// written in this shape is picked up by the S3 gateway and authenticates, and
// `weed shell`'s s3.configure lists it alongside its own.
type s3Identity struct {
	Name        string         `json:"name"`
	Credentials []s3Credential `json:"credentials"`
	Actions     []string       `json:"actions"`
}

type s3Credential struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Status    string `json:"status"`
}

// FilerCredentialIssuer provisions per-project S3 identities over the filer's
// HTTP API, with no `weed` binary.
//
// Replaces shelling out to `weed shell`. That worked but made the CLI a hard
// prerequisite for onboarding, and against a containerized cluster a
// host-native `weed` HANGS rather than failing — the same command succeeds
// inside the container, so the failure is silent and unexplainable to a user.
//
// The identity's actions are Read/Write on exactly one bucket
// ("Read:silo-<project>"), so a project's credential cannot reach another
// project's bucket. That scoping is the enforced isolation boundary, and it is
// unchanged from the shell implementation — only the transport differs.
type FilerCredentialIssuer struct {
	filer  string // host:port of the filer's HTTP API
	client *http.Client
	secret SecretStore
}

var _ CredentialIssuer = (*FilerCredentialIssuer)(nil)

// NewFilerCredentialIssuer builds an issuer against a filer's HTTP API.
// secretStore is where issued secret keys are persisted; pass nil to skip
// persistence (tests).
func NewFilerCredentialIssuer(filer string, secretStore SecretStore) *FilerCredentialIssuer {
	filer = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(filer, "http://"), "https://"), "/")
	if filer == "" {
		filer = "localhost:8888"
	}
	return &FilerCredentialIssuer{
		filer:  filer,
		client: &http.Client{Timeout: 15 * time.Second},
		secret: secretStore,
	}
}

// Issue creates a bucket-scoped identity and returns its credentialID.
func (f *FilerCredentialIssuer) Issue(ctx context.Context, projectID, bucket string) (string, error) {
	credID := credentialName(projectID)
	accessKey, err := randKey(20)
	if err != nil {
		return "", fmt.Errorf("admin: generate access key: %w", err)
	}
	secretKey, err := randKey(40)
	if err != nil {
		return "", fmt.Errorf("admin: generate secret key: %w", err)
	}

	identity := s3Identity{
		Name:        credID,
		Credentials: []s3Credential{{AccessKey: accessKey, SecretKey: secretKey, Status: "Active"}},
		// Scoped to one bucket. This is the isolation boundary: without the
		// ":bucket" suffix the identity could read every project's memory.
		Actions: []string{"Read:" + bucket, "Write:" + bucket},
	}
	body, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return "", fmt.Errorf("admin: encode identity for %q: %w", projectID, err)
	}

	if err := f.put(ctx, credID+".json", body); err != nil {
		return "", fmt.Errorf("admin: issue credential for %q: %w", projectID, err)
	}

	// Persist access+secret before returning; on failure the caller's rollback
	// revokes the identity just created.
	if f.secret != nil {
		if err := f.secret.PutSecret(ctx, credID, accessKey+":"+secretKey); err != nil {
			return "", fmt.Errorf("admin: store secret for %q: %w", projectID, err)
		}
	}
	return credID, nil
}

// Revoke deletes the identity and drops the stored secret. Deleting an absent
// identity is a no-op, so teardown and onboarding-rollback stay idempotent.
func (f *FilerCredentialIssuer) Revoke(ctx context.Context, credentialID string) error {
	if credentialID == "" {
		return nil
	}
	if err := f.delete(ctx, credentialID+".json"); err != nil {
		return fmt.Errorf("admin: revoke credential %q: %w", credentialID, err)
	}
	if f.secret != nil {
		if err := f.secret.DeleteSecret(ctx, credentialID); err != nil {
			return fmt.Errorf("admin: delete stored secret %q: %w", credentialID, err)
		}
	}
	return nil
}

// Probe reports whether identities can be managed, without creating anything.
//
// Listing the identity directory exercises the same host, port, and path that
// Issue uses, so a passing probe means onboarding's credential step will work.
func (f *FilerCredentialIssuer) Probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url(""), nil)
	if err != nil {
		return err
	}
	// Without this the filer renders an HTML directory listing.
	req.Header.Set("Accept", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("admin: filer %s unreachable: %w", f.filer, err)
	}
	defer resp.Body.Close()
	// 404 is fine: the directory is created on the first identity written.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("admin: filer %s returned %d listing %s",
			f.filer, resp.StatusCode, identityDir)
	}
	return nil
}

func (f *FilerCredentialIssuer) url(name string) string {
	if name == "" {
		return "http://" + f.filer + identityDir + "/"
	}
	return "http://" + f.filer + identityDir + "/" + name
}

// put writes an identity file. The filer's HTTP API takes a multipart upload,
// the same shape `weed filer.copy` uses.
func (f *FilerCredentialIssuer) put(ctx context.Context, name string, content []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	if _, err := part.Write(content); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url(name), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("filer %s unreachable: %w", f.filer, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("filer returned %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

func (f *FilerCredentialIssuer) delete(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, f.url(name), nil)
	if err != nil {
		return err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("filer %s unreachable: %w", f.filer, err)
	}
	defer resp.Body.Close()
	// 404 means it is already gone, which is what the caller wanted.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusNotFound {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("filer returned %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}
