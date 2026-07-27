package admin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// capturingFiler records what the issuer sent, so the identity document can be
// asserted without a live cluster.
type capturingFiler struct {
	*httptest.Server
	lastPath   string
	lastMethod string
	lastBody   []byte
	status     int
}

func newCapturingFiler(t *testing.T) *capturingFiler {
	t.Helper()
	c := &capturingFiler{status: http.StatusCreated}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.lastPath, c.lastMethod = r.URL.Path, r.Method
		// A real filer answers DELETE with 204, not the 201 it gives a write.
		// Mirroring that keeps the fixture honest about what the code must accept.
		if r.Method == http.MethodDelete && c.status == http.StatusCreated {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseMultipartForm(1 << 20); err == nil {
				if f, _, err := r.FormFile("file"); err == nil {
					c.lastBody, _ = io.ReadAll(f)
					f.Close()
				}
			}
		}
		w.WriteHeader(c.status)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *capturingFiler) issuer() *FilerCredentialIssuer {
	return NewFilerCredentialIssuer(strings.TrimPrefix(c.URL, "http://"), nil)
}

// The identity must be scoped to one bucket. Without the ":bucket" suffix the
// credential could read every project's memory — this is the isolation
// boundary, not a nicety.
func TestFilerIssuer_ScopesTheIdentityToOneBucket(t *testing.T) {
	f := newCapturingFiler(t)
	credID, err := f.issuer().Issue(context.Background(), "proj-11", "silo-proj-11")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if credID != "silo-cred-proj-11" {
		t.Errorf("credentialID = %q", credID)
	}

	var id s3Identity
	if err := json.Unmarshal(f.lastBody, &id); err != nil {
		t.Fatalf("decode identity: %v (body %q)", err, f.lastBody)
	}
	want := map[string]bool{"Read:silo-proj-11": true, "Write:silo-proj-11": true}
	if len(id.Actions) != 2 {
		t.Fatalf("actions = %v, want exactly Read and Write on one bucket", id.Actions)
	}
	for _, a := range id.Actions {
		if !want[a] {
			t.Errorf("action %q is not scoped to silo-proj-11", a)
		}
		// An unsuffixed action grants the whole cluster.
		if !strings.Contains(a, ":") {
			t.Errorf("action %q has no bucket scope — it would grant every bucket", a)
		}
	}
	if id.Name != credID {
		t.Errorf("identity name = %q, want the credentialID so Revoke can find it", id.Name)
	}
}

// Keys must be unguessable: they are live S3 credentials.
func TestFilerIssuer_GeneratesStrongDistinctKeys(t *testing.T) {
	f := newCapturingFiler(t)
	iss := f.issuer()

	seen := map[string]bool{}
	for range 5 {
		if _, err := iss.Issue(context.Background(), "proj-11", "silo-proj-11"); err != nil {
			t.Fatalf("Issue: %v", err)
		}
		var id s3Identity
		if err := json.Unmarshal(f.lastBody, &id); err != nil {
			t.Fatalf("decode: %v", err)
		}
		c := id.Credentials[0]
		if len(c.AccessKey) < 32 || len(c.SecretKey) < 64 {
			t.Errorf("keys too short: access=%d secret=%d", len(c.AccessKey), len(c.SecretKey))
		}
		if c.AccessKey == c.SecretKey {
			t.Error("access and secret key are identical")
		}
		if seen[c.AccessKey] || seen[c.SecretKey] {
			t.Fatal("a key was reused across issues")
		}
		seen[c.AccessKey], seen[c.SecretKey] = true, true
		if c.Status != "Active" {
			t.Errorf("status = %q, want Active", c.Status)
		}
	}
}

func TestFilerIssuer_WritesToTheIdentityPath(t *testing.T) {
	f := newCapturingFiler(t)
	if _, err := f.issuer().Issue(context.Background(), "proj-11", "silo-proj-11"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	want := identityDir + "/silo-cred-proj-11.json"
	if f.lastPath != want {
		t.Errorf("wrote to %q, want %q", f.lastPath, want)
	}
	if f.lastMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", f.lastMethod)
	}
}

// Revoking an absent identity must be a no-op: teardown and onboarding-rollback
// both call it on paths that may already be gone.
func TestFilerIssuer_RevokeIsIdempotent(t *testing.T) {
	f := newCapturingFiler(t)
	f.status = http.StatusNotFound
	iss := f.issuer()

	for i := range 3 {
		if err := iss.Revoke(context.Background(), "silo-cred-gone"); err != nil {
			t.Errorf("revoke #%d on an absent identity should be a no-op: %v", i+1, err)
		}
	}
	if f.lastMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", f.lastMethod)
	}
	// An empty credentialID means nothing was ever issued.
	if err := iss.Revoke(context.Background(), ""); err != nil {
		t.Errorf("revoking an empty credentialID should be a no-op: %v", err)
	}
}

// A filer that rejects the write must fail loudly: onboarding's rollback
// depends on knowing the credential step did not succeed.
func TestFilerIssuer_ReportsFilerFailures(t *testing.T) {
	f := newCapturingFiler(t)
	f.status = http.StatusInternalServerError

	if _, err := f.issuer().Issue(context.Background(), "proj-11", "silo-proj-11"); err == nil {
		t.Error("a 500 from the filer must be reported, not swallowed")
	}
}

// An unreachable filer must fail promptly rather than hang — the exact failure
// mode that made `weed shell` unusable.
func TestFilerIssuer_UnreachableFilerFailsFast(t *testing.T) {
	// A port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot allocate a port")
	}
	addr := l.Addr().String()
	l.Close()

	iss := NewFilerCredentialIssuer(addr, nil)
	start := time.Now()
	if _, err := iss.Issue(context.Background(), "proj-11", "silo-proj-11"); err == nil {
		t.Fatal("an unreachable filer must be an error")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("took %v to fail; it must not hang the way `weed shell` did", elapsed)
	}
}

func TestFilerIssuer_ProbeChecksReachability(t *testing.T) {
	f := newCapturingFiler(t)
	f.status = http.StatusOK
	if err := f.issuer().Probe(context.Background()); err != nil {
		t.Errorf("Probe against a healthy filer: %v", err)
	}

	// 404 is fine: the directory appears with the first identity written.
	f.status = http.StatusNotFound
	if err := f.issuer().Probe(context.Background()); err != nil {
		t.Errorf("Probe should accept 404 on an empty cluster: %v", err)
	}

	f.status = http.StatusInternalServerError
	if err := f.issuer().Probe(context.Background()); err == nil {
		t.Error("Probe should report a filer returning 500")
	}
}

func TestNewFilerCredentialIssuer_NormalizesAddress(t *testing.T) {
	for _, in := range []string{"localhost:8888", "http://localhost:8888", "http://localhost:8888/"} {
		iss := NewFilerCredentialIssuer(in, nil)
		if iss.filer != "localhost:8888" {
			t.Errorf("NewFilerCredentialIssuer(%q).filer = %q", in, iss.filer)
		}
	}
	if iss := NewFilerCredentialIssuer("", nil); iss.filer != "localhost:8888" {
		t.Errorf("empty address should default, got %q", iss.filer)
	}
}

// memSecretStore is an in-memory SecretStore for tests.
type memSecretStore struct{ m map[string]string }

func newMemSecretStore() *memSecretStore { return &memSecretStore{m: map[string]string{}} }

func (s *memSecretStore) PutSecret(_ context.Context, id, secret string) error {
	s.m[id] = secret
	return nil
}

func (s *memSecretStore) DeleteSecret(_ context.Context, id string) error {
	delete(s.m, id)
	return nil
}

// The secret key is persisted to the SecretStore and never returned through the
// registry, which only ever holds the credentialID as a reference.
func TestFilerIssuer_PersistsTheSecretOutsideTheRegistry(t *testing.T) {
	f := newCapturingFiler(t)
	store := newMemSecretStore()
	iss := NewFilerCredentialIssuer(strings.TrimPrefix(f.URL, "http://"), store)

	credID, err := iss.Issue(context.Background(), "proj-11", "silo-proj-11")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	secret, ok := store.m[credID]
	if !ok {
		t.Fatal("the secret was not persisted")
	}
	if !strings.Contains(secret, ":") {
		t.Errorf("stored secret %q should be access:secret", secret)
	}

	if err := iss.Revoke(context.Background(), credID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, still := store.m[credID]; still {
		t.Error("revoking must also drop the stored secret")
	}
}
