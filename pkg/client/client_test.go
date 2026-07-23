package client_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/tooltropolis/silo/internal/cache"
	"github.com/tooltropolis/silo/internal/daemon"
	"github.com/tooltropolis/silo/internal/testsupport"
	"github.com/tooltropolis/silo/pkg/client"
)

// newTestStack stands up a real Daemon + HTTP server over an in-memory backend
// and returns SDK clients scoped to two different projects, exercising the SDK
// against the actual daemon surface rather than a mock.
func newTestStack(t *testing.T) (aClient, bClient client.Client) {
	t.Helper()

	be := testsupport.NewMemBackend()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	d := daemon.New(be, c, nil, nil)
	// Two tokens, each scoped to exactly one project — the authorization
	// boundary the SDK relies on.
	verifier := daemon.StaticTokenVerifier{
		"token-a": "proj-a",
		"token-b": "proj-b",
	}
	srv := httptest.NewServer(daemon.NewServer(d, verifier).Handler())
	t.Cleanup(srv.Close)

	mk := func(token string) client.Client {
		cl, err := client.New(client.Config{Endpoint: srv.URL, Token: token})
		if err != nil {
			t.Fatalf("client.New: %v", err)
		}
		return cl
	}
	return mk("token-a"), mk("token-b")
}

func TestClient_WriteReadRoundTrip(t *testing.T) {
	ctx := context.Background()
	a, _ := newTestStack(t)

	want := []byte("# Notes\n\nremember this")
	if err := a.Write(ctx, "memory/notes.md", want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := a.Read(ctx, "memory/notes.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Read: want %q, got %q", want, got)
	}
}

func TestClient_ReadMissingIsNotFound(t *testing.T) {
	a, _ := newTestStack(t)
	if _, err := a.Read(context.Background(), "nope.md"); !errors.Is(err, client.ErrNotFound) {
		t.Fatalf("Read missing: want ErrNotFound, got %v", err)
	}
}

func TestClient_List(t *testing.T) {
	ctx := context.Background()
	a, _ := newTestStack(t)

	for _, p := range []string{"memory/a.md", "memory/b.md", "other/c.md"} {
		if err := a.Write(ctx, p, []byte("x")); err != nil {
			t.Fatalf("Write %s: %v", p, err)
		}
	}
	paths, err := a.List(ctx, "memory/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("List memory/: want 2 paths, got %d (%v)", len(paths), paths)
	}
	// The prefix must actually filter.
	all, err := a.List(ctx, "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List all: want 3, got %d", len(all))
	}
}

func TestClient_Search(t *testing.T) {
	ctx := context.Background()
	a, _ := newTestStack(t)

	if err := a.Write(ctx, "memory/one.md", []byte("the user prefers dark mode")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := a.Write(ctx, "memory/two.md", []byte("unrelated content")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	hits, err := a.Search(ctx, "memory/", "DARK MODE") // case-insensitive
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("Search: want 1 hit, got %d (%v)", len(hits), hits)
	}
	if hits[0].Path != "memory/one.md" {
		t.Fatalf("Search: wrong path %q", hits[0].Path)
	}
	if hits[0].Snippet == "" {
		t.Fatal("Search: empty snippet")
	}
}

// TestClient_ProjectIsolation is the SDK-level isolation guarantee: a token
// scoped to project A cannot read project B's memory, even at the same path.
func TestClient_ProjectIsolation(t *testing.T) {
	ctx := context.Background()
	a, b := newTestStack(t)

	const path = "memory/secret.md"
	if err := a.Write(ctx, path, []byte("project A's private note")); err != nil {
		t.Fatalf("A write: %v", err)
	}

	// B reads the SAME path and must not see A's content.
	got, err := b.Read(ctx, path)
	if err == nil {
		t.Fatalf("cross-project read leaked A's data to B: %q", got)
	}
	if !errors.Is(err, client.ErrNotFound) {
		t.Fatalf("B read: want ErrNotFound, got %v", err)
	}

	// B writes its own content at the same path; A must still see only its own.
	if err := b.Write(ctx, path, []byte("project B's note")); err != nil {
		t.Fatalf("B write: %v", err)
	}
	aGot, err := a.Read(ctx, path)
	if err != nil {
		t.Fatalf("A re-read: %v", err)
	}
	if string(aGot) != "project A's private note" {
		t.Fatalf("A's content was overwritten across projects: %q", aGot)
	}
}

func TestClient_RejectsBadToken(t *testing.T) {
	be := testsupport.NewMemBackend()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	d := daemon.New(be, c, nil, nil)
	srv := httptest.NewServer(daemon.NewServer(d, daemon.StaticTokenVerifier{"good": "proj-a"}).Handler())
	t.Cleanup(srv.Close)

	bad, err := client.New(client.Config{Endpoint: srv.URL, Token: "bogus"})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	if _, err := bad.Read(context.Background(), "memory/x.md"); !errors.Is(err, client.ErrUnauthorized) {
		t.Fatalf("unknown token: want ErrUnauthorized, got %v", err)
	}
}

func TestClient_RequiresEndpointAndToken(t *testing.T) {
	if _, err := client.New(client.Config{Token: "t"}); err == nil {
		t.Fatal("missing endpoint should error")
	}
	if _, err := client.New(client.Config{Endpoint: "http://x"}); err == nil {
		t.Fatal("missing token should error")
	}
}
