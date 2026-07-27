package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tooltropolis/silo/internal/cache"
)

// scopeTestProject is the single project both scoped tokens resolve to.
const scopeTestProject = "proj-a"

// scopedVerifier resolves two tokens: one read-write, one read-only, both for
// the same project. Same project on purpose — it isolates scope as the only
// variable, so a difference in behaviour cannot be explained by tenancy.
type scopedVerifier struct{ project string }

func (v scopedVerifier) ProjectFor(token string) (Grant, error) {
	switch token {
	case "rw":
		return Grant{ProjectID: v.project}, nil
	case "ro":
		return Grant{ProjectID: v.project, ReadOnly: true}, nil
	}
	return Grant{}, ErrUnauthorized
}

// newScopedServer mirrors newHTTPFixture but swaps in a verifier that can
// express scope, which StaticTokenVerifier deliberately cannot.
func newScopedServer(t *testing.T) http.Handler {
	t.Helper()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	d := New(&fakeBackend{}, c, newGenRegistry(), nil)
	return NewServer(d, scopedVerifier{project: scopeTestProject}).Handler()
}

func writeBody(path, content string) string {
	b, _ := json.Marshal(writeRequest{Path: path, Content: []byte(content)})
	return string(b)
}

// The security property: a read-only token cannot write. Anthropic's memory
// docs are explicit that a prompt-injected agent writing to memory is the
// hazard read-only access exists to remove — persuasion is not a control.
func TestReadOnlyToken_IsRefusedOnWrite(t *testing.T) {
	h := newScopedServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/write",
		strings.NewReader(writeBody("memory/injected.md", "malicious")))
	req.Header.Set("Authorization", "Bearer ro")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// 403, not 401: the token authenticated. A 401 tells the client to
	// re-authenticate, which can never help.
	if rec.Code != http.StatusForbidden {
		t.Fatalf("write with a read-only token = %d, want 403\nbody: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "read-only") {
		t.Errorf("the error should say the token is read-only, got %q", rec.Body.String())
	}
}

// The refusal must not be a 500: an agent has to be able to tell "you may not"
// from "something broke", or it will retry a write that can never succeed.
func TestReadOnlyToken_RefusalIsNotAServerError(t *testing.T) {
	h := newScopedServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/write",
		strings.NewReader(writeBody("memory/x.md", "x")))
	req.Header.Set("Authorization", "Bearer ro")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code >= 500 {
		t.Errorf("a scope refusal surfaced as %d — it is a policy decision, not a fault", rec.Code)
	}
}

// A read-only token must still read, list, and search. A token that cannot read
// is not read-only, it is useless.
func TestReadOnlyToken_CanStillRead(t *testing.T) {
	h := newScopedServer(t)

	// Seed with the read-write token so there is something to read back.
	seed := httptest.NewRequest(http.MethodPost, "/v1/write",
		strings.NewReader(writeBody("memory/notes.md", "hello")))
	seed.Header.Set("Authorization", "Bearer rw")
	seedRec := httptest.NewRecorder()
	h.ServeHTTP(seedRec, seed)
	if seedRec.Code != http.StatusOK {
		t.Fatalf("seeding write with a read-write token = %d, want 200\nbody: %s",
			seedRec.Code, seedRec.Body)
	}

	for _, route := range []string{
		"/v1/read?path=memory/notes.md",
		"/v1/list?prefix=memory/",
		"/v1/search?prefix=memory/&q=hello",
	} {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		req.Header.Set("Authorization", "Bearer ro")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s with a read-only token = %d, want 200\nbody: %s",
				route, rec.Code, rec.Body)
		}
	}
}

// Nothing partial may be stored. A refusal that still wrote the content would
// be worse than no check at all, since the operator would believe it held.
func TestReadOnlyToken_RefusedWriteStoresNothing(t *testing.T) {
	h := newScopedServer(t)
	const path = "memory/never-written.md"

	req := httptest.NewRequest(http.MethodPost, "/v1/write",
		strings.NewReader(writeBody(path, "should not persist")))
	req.Header.Set("Authorization", "Bearer ro")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("setup: want 403, got %d", rec.Code)
	}

	// Read it back with the READ-WRITE token: if anything was stored, this sees
	// it. Checking with the read-only token would prove nothing.
	get := httptest.NewRequest(http.MethodGet, "/v1/read?path="+path, nil)
	get.Header.Set("Authorization", "Bearer rw")
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, get)

	if getRec.Code == http.StatusOK {
		t.Errorf("a refused write left content behind: %s", getRec.Body)
	}
}

// A read-write token must be unaffected. The regression this guards is a check
// written too broadly that refuses every write.
func TestReadWriteToken_StillWrites(t *testing.T) {
	h := newScopedServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/write",
		strings.NewReader(writeBody("memory/ok.md", "fine")))
	req.Header.Set("Authorization", "Bearer rw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("write with a read-write token = %d, want 200\nbody: %s", rec.Code, rec.Body)
	}
}

// authorizeWrite is the enforcement point; assert it directly so the property
// is pinned even if the HTTP layer is restructured.
func TestAuthorizeWrite_RejectsReadOnlyGrant(t *testing.T) {
	s := newScopedServerRaw(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/write", nil)
	req.Header.Set("Authorization", "Bearer ro")
	if _, err := s.authorizeWrite(req); !errors.Is(err, ErrReadOnlyToken) {
		t.Errorf("authorizeWrite with a read-only token = %v, want ErrReadOnlyToken", err)
	}

	req.Header.Set("Authorization", "Bearer rw")
	grant, err := s.authorizeWrite(req)
	if err != nil {
		t.Fatalf("authorizeWrite with a read-write token: %v", err)
	}
	if grant.ProjectID != scopeTestProject {
		t.Errorf("grant.ProjectID = %q, want %q", grant.ProjectID, scopeTestProject)
	}
}

// A read-only token still resolves to exactly one project: scope restricts what
// may be done, never widens who may be reached.
func TestReadOnlyToken_IsStillProjectScoped(t *testing.T) {
	s := newScopedServerRaw(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/read", nil)
	req.Header.Set("Authorization", "Bearer ro")
	grant, err := s.authorize(req)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if grant.ProjectID != scopeTestProject {
		t.Errorf("ISOLATION: read-only grant resolved to %q, want %q", grant.ProjectID, scopeTestProject)
	}
	if !grant.ReadOnly {
		t.Error("the read-only token did not carry its scope")
	}
}

// newScopedServerRaw returns the *Server itself, for tests that assert on
// authorize/authorizeWrite directly rather than through the HTTP surface.
func newScopedServerRaw(t *testing.T) *Server {
	t.Helper()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return NewServer(New(&fakeBackend{}, c, newGenRegistry(), nil),
		scopedVerifier{project: scopeTestProject})
}
