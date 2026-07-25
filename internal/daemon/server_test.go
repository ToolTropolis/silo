package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tooltropolis/silo/internal/cache"
)

func newHTTPFixture(t *testing.T, be *fakeBackend) *httptest.Server {
	t.Helper()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	d := New(be, c, nil, nil)
	s := NewServer(d, StaticTokenVerifier{"tok-a": "proj-a"})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func postWrite(t *testing.T, srv *httptest.Server, token, path, content string) (int, map[string]any) {
	t.Helper()
	body := strings.NewReader(`{"path":"` + path + `","content":"` +
		base64.StdEncoding.EncodeToString([]byte(content)) + `"}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/write", body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v1/write: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestHandleWrite_StatusReflectsDurability is the API contract this whole change
// exists for. A write that only reached local disk previously returned
// 200 {"status":"ok"} — identical to a durable write — so a caller had no way to
// know its memory was one disk failure from gone.
func TestHandleWrite_StatusReflectsDurability(t *testing.T) {
	cases := []struct {
		name        string
		backendDown bool
		wantStatus  int
		wantDurable bool
		wantBody    string
	}{
		{"durable when the backend is up", false, http.StatusOK, true, "ok"},
		{"queued when the backend is down", true, http.StatusAccepted, false, "queued"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := &fakeBackend{}
			if tc.backendDown {
				be.getErr = errors.New("connection refused")
			}
			srv := newHTTPFixture(t, be)

			code, body := postWrite(t, srv, "tok-a", "memory/x.md", "hello")
			if code != tc.wantStatus {
				t.Errorf("status = %d, want %d", code, tc.wantStatus)
			}
			if got, _ := body["durable"].(bool); got != tc.wantDurable {
				t.Errorf("durable = %v, want %v", got, tc.wantDurable)
			}
			if got, _ := body["status"].(string); got != tc.wantBody {
				t.Errorf("status field = %q, want %q", got, tc.wantBody)
			}
		})
	}
}

// A queued write is still a 2xx: it was accepted and will be replayed. Callers
// that only check for success must not see it as a failure and retry, since
// retrying re-queues the same content.
func TestHandleWrite_QueuedIsSuccessNotError(t *testing.T) {
	be := &fakeBackend{getErr: errors.New("connection refused")}
	srv := newHTTPFixture(t, be)

	code, _ := postWrite(t, srv, "tok-a", "memory/x.md", "hello")
	if code < 200 || code >= 300 {
		t.Errorf("status = %d, want a 2xx — a queued write is accepted, not failed", code)
	}
}

// The token remains the only thing that decides which project is written, so a
// caller cannot address another silo regardless of the body.
func TestHandleWrite_RejectsUnknownToken(t *testing.T) {
	srv := newHTTPFixture(t, &fakeBackend{})
	code, _ := postWrite(t, srv, "not-a-token", "memory/x.md", "hello")
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}

// TestHandleQueue_ReportsOwnProjectOnly is the isolation guarantee for the new
// endpoint: the project comes from the token, and no request parameter can
// redirect it at someone else's silo.
func TestHandleQueue_ReportsOwnProjectOnly(t *testing.T) {
	be := &fakeBackend{getErr: errors.New("connection refused")}
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	d := New(be, c, nil, nil)
	s := NewServer(d, StaticTokenVerifier{"tok-a": "proj-a", "tok-b": "proj-b"})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// Queue two writes for proj-a only.
	for _, p := range []string{"memory/1.md", "memory/2.md"} {
		if _, err := d.SafeWrite(context.Background(), "proj-a", p,
			func([]byte) []byte { return []byte("x") }, "agent", "s"); err != nil {
			t.Fatalf("queueing: %v", err)
		}
	}

	get := func(token, query string) queueResponse {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/queue"+query, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET /v1/queue: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var out queueResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	if got := get("tok-a", ""); got.Project != "proj-a" || got.Pending != 2 {
		t.Errorf("proj-a queue = %+v, want project proj-a with 2 pending", got)
	}
	if got := get("tok-b", ""); got.Project != "proj-b" || got.Pending != 0 {
		t.Errorf("proj-b queue = %+v, want project proj-b with 0 pending", got)
	}
	// A project parameter must not redirect the answer.
	if got := get("tok-b", "?project=proj-a"); got.Project != "proj-b" || got.Pending != 0 {
		t.Errorf("?project=proj-a with proj-b's token returned %+v — the token must decide the project", got)
	}
}
