package admin

import (
	"net/http"
	"net/url"
	"testing"
)

// Every route on this surface is authenticated when a token is configured.
// Enumerated rather than spot-checked: a route added later without auth is
// exactly the mistake worth failing a build over, and this surface can destroy
// a project.
func TestAuth_TokenRequiredOnEveryRoute(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
		Daemon:   &fakeDaemon{},
		Prov:     &fakeProvisioner{},
		Token:    "secret",
	})

	gets := []string{"/", "/settings", "/projects"}
	for _, path := range gets {
		resp, err := noRedirectClient().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, resp.StatusCode)
		}
	}

	posts := []string{"/cache-action", "/onboard", "/teardown", "/settings"}
	for _, path := range posts {
		resp := postForm(t, ts, path, url.Values{"project": {"proj-11"}})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST %s without a token = %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestAuth_WrongTokenIsRejected(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Token: "secret"})

	for _, header := range []string{"", "Bearer", "Bearer wrong", "secret", "Bearer secretx", "Bearer secre"} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q = %d, want 401", header, resp.StatusCode)
		}
	}
}

func TestAuth_CorrectTokenIsAccepted(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
		Token:    "secret",
	})

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// healthz stays open so a supervisor can check liveness without being handed a
// credential that could tear down a project.
func TestAuth_HealthzIsOpen(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Token: "secret"})

	resp, err := noRedirectClient().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200 without a token", resp.StatusCode)
	}
}

// No token configured means a Unix socket, where the filesystem permissions are
// the boundary. silo-admin refuses to bind TCP without a token, so this is not
// an open door — but the handler must still serve.
func TestAuth_NoTokenServesUnauthenticated(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})

	resp, err := noRedirectClient().Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 on a socket-style listener", resp.StatusCode)
	}
}

// "/" is a catch-all in net/http's mux, so an unknown path would otherwise
// render the cache view. A console that answers 200 for /destroy-everything
// reads as if that action exists.
func TestRouting_UnknownPathIs404(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})

	for _, path := range []string{"/nope", "/destroy-everything", "/settings/extra"} {
		resp, err := noRedirectClient().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

// Mutating routes must reject GET, so a link or a prefetch cannot trigger one.
func TestRouting_MutationsRejectGET(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
		Daemon:   &fakeDaemon{},
		Prov:     &fakeProvisioner{},
	})

	for _, path := range []string{"/cache-action", "/onboard", "/teardown"} {
		resp, err := noRedirectClient().Get(ts.URL + path + "?project=proj-11&action=purge")
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, resp.StatusCode)
		}
	}
}
