package admin

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tooltropolis/silo/internal/registry"
)

// fakeMinter issues predictable tokens so a test can assert on them.
type fakeMinter struct {
	mu      sync.Mutex
	minted  []string // "project:label"
	next    string
	err     error
	tokens  []registry.AgentToken
	revoked []string
	// lastReadOnly records the scope the console asked for, so a test can
	// assert that a read-only mint actually reaches the store.
	lastReadOnly bool
}

func (f *fakeMinter) MintToken(_ context.Context, projectID, label, _ string, readOnly bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.lastReadOnly = readOnly
	f.minted = append(f.minted, projectID+":"+label)
	if f.next == "" {
		f.next = "silo_pat_TESTTOKEN"
	}
	return f.next, nil
}

func (f *fakeMinter) ListTokens(context.Context, string) ([]registry.AgentToken, error) {
	return f.tokens, nil
}

func (f *fakeMinter) RevokeToken(_ context.Context, hash string) error {
	f.revoked = append(f.revoked, hash)
	return nil
}

func connectFixture(t *testing.T, minter TokenMinter) *httptest.Server {
	t.Helper()
	ts := newFixture(t, Config{
		Registry:        activeProject("proj-11"),
		Settings:        newFakeSettings(nil),
		Prov:            &fakeProvisioner{},
		Tokens:          minter,
		AgentDaemonAddr: "http://127.0.0.1:8500",
		MCPBinary:       "silo-mcp",
	})
	return ts
}

// The connect step is where a repo gets wired up, so it must show the config
// even before anything is minted or written.
func TestConnect_ShowsConfigWithoutMinting(t *testing.T) {
	ts := connectFixture(t, &fakeMinter{})

	body := getBody(t, ts, "/onboard/connect?project=proj-11")
	if !strings.Contains(body, "mcpServers") {
		t.Error("the connect step should preview .mcp.json")
	}
	if !strings.Contains(body, "${SILO_TOKEN}") {
		t.Error("the config should reference the token from the environment")
	}
	if !strings.Contains(body, "proj-11") {
		t.Error("the config should be scoped to the project")
	}
}

// A token exists in plaintext only at creation. It must be shown, and shown
// as a one-time reveal.
func TestConnect_MintRevealsTheTokenOnce(t *testing.T) {
	minter := &fakeMinter{next: "silo_pat_SECRETVALUE"}
	ts := connectFixture(t, minter)

	resp := postForm(t, ts, "/onboard/mint", url.Values{
		"project": {"proj-11"}, "label": {"laptop"},
	})
	loc := resp.Header.Get("Location")

	// The token must never travel in a URL: that lands in browser history, the
	// referer header, and any proxy log in between.
	if strings.Contains(loc, "SECRETVALUE") {
		t.Fatalf("the token leaked into the redirect URL: %s", loc)
	}
	if len(minter.minted) != 1 || minter.minted[0] != "proj-11:laptop" {
		t.Errorf("minted = %v, want [proj-11:laptop]", minter.minted)
	}

	body := getBody(t, ts, "/onboard/connect?project=proj-11")
	if !strings.Contains(body, "silo_pat_SECRETVALUE") {
		t.Error("the freshly minted token should be revealed once")
	}
	if !strings.Contains(body, "Copy this token now") {
		t.Error("the reveal should be unmissable — it cannot be shown again")
	}
}

func TestConnect_MintDefaultsTheLabel(t *testing.T) {
	minter := &fakeMinter{}
	ts := connectFixture(t, minter)

	postForm(t, ts, "/onboard/mint", url.Values{"project": {"proj-11"}})
	if len(minter.minted) != 1 || !strings.HasSuffix(minter.minted[0], ":agent") {
		t.Errorf("minted = %v, want a default label", minter.minted)
	}
}

func TestConnect_MintFailureIsReported(t *testing.T) {
	minter := &fakeMinter{err: errors.New("rqlite unreachable")}
	ts := connectFixture(t, minter)

	resp := postForm(t, ts, "/onboard/mint", url.Values{"project": {"proj-11"}})
	if _, errMsg := flashOf(t, resp); !strings.Contains(errMsg, "rqlite unreachable") {
		t.Errorf("error = %q, want the failure surfaced", errMsg)
	}
}

// Without a token store the step still works for copy-by-hand, and says why
// minting is unavailable rather than silently omitting the button.
func TestConnect_NoMinterSaysSo(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
	})

	body := getBody(t, ts, "/onboard/connect?project=proj-11")
	if !strings.Contains(body, "No token store configured") {
		t.Error("the step should explain why it cannot mint")
	}
	if !strings.Contains(body, "mcpServers") {
		t.Error("the config should still be shown for copying")
	}
}

// Previewing a real path shows what the write would do before offering it.
func TestConnect_PreviewsAWrite(t *testing.T) {
	dir := repoDir(t)
	ts := connectFixture(t, &fakeMinter{})

	body := getBody(t, ts, "/onboard/connect?project=proj-11&repo="+url.QueryEscape(dir))
	if !strings.Contains(body, "will be created") {
		t.Error("the preview should say the file will be created")
	}
	if !strings.Contains(body, filepath.Join(dir, ".mcp.json")) {
		t.Error("the preview should name the exact destination path")
	}
}

func TestConnect_BadPathIsReportedInline(t *testing.T) {
	ts := connectFixture(t, &fakeMinter{})

	body := getBody(t, ts, "/onboard/connect?project=proj-11&repo=relative/path")
	if !strings.Contains(body, "absolute") {
		t.Error("a bad path should be explained on the step, not hidden")
	}
}

func TestConnect_WritesTheFile(t *testing.T) {
	dir := repoDir(t)
	ts := connectFixture(t, &fakeMinter{})

	resp := postForm(t, ts, "/onboard/write", url.Values{
		"project": {"proj-11"}, "repo": {dir},
	})
	flash, errMsg := flashOf(t, resp)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if !strings.Contains(flash, ".mcp.json") {
		t.Errorf("flash = %q, want it to name the written file", flash)
	}

	raw, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("the file was not written: %v", err)
	}
	if !strings.Contains(string(raw), "proj-11") {
		t.Error("the written config should be scoped to the project")
	}
	if strings.Contains(string(raw), "silo_pat_") {
		t.Error("a literal token must never be written to disk")
	}
}

// Replacing an existing silo entry needs a second, explicit confirmation: the
// operator may be pointing at the wrong repo.
func TestConnect_ReplacingRequiresConfirmation(t *testing.T) {
	dir := repoDir(t)
	existing := `{"mcpServers":{"silo":{"command":"silo-mcp","env":{"SILO_PROJECT":"otherproject"}}}}`
	path := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ts := connectFixture(t, &fakeMinter{})

	resp := postForm(t, ts, "/onboard/write", url.Values{
		"project": {"proj-11"}, "repo": {dir},
	})
	if _, errMsg := flashOf(t, resp); !strings.Contains(errMsg, "replace") {
		t.Errorf("error = %q, want it to demand confirmation", errMsg)
	}
	// The existing entry must be untouched.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "otherproject") {
		t.Error("the existing entry was overwritten without confirmation")
	}

	// With confirmation it goes through.
	resp = postForm(t, ts, "/onboard/write", url.Values{
		"project": {"proj-11"}, "repo": {dir}, "confirm_replace": {"yes"},
	})
	if _, errMsg := flashOf(t, resp); errMsg != "" {
		t.Fatalf("confirmed replace failed: %s", errMsg)
	}
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), "proj-11") {
		t.Error("the confirmed replace did not happen")
	}
}

func TestConnect_WriteRequiresProjectAndRepo(t *testing.T) {
	ts := connectFixture(t, &fakeMinter{})

	for _, form := range []url.Values{
		{"project": {"proj-11"}},
		{"repo": {"/tmp"}},
		{},
	} {
		resp := postForm(t, ts, "/onboard/write", form)
		if _, errMsg := flashOf(t, resp); errMsg == "" {
			t.Errorf("form %v should be rejected", form)
		}
	}
}

// Mutating routes must reject GET, so a prefetch cannot mint a credential or
// write into a repo.
func TestConnect_MutationsRejectGET(t *testing.T) {
	minter := &fakeMinter{}
	ts := connectFixture(t, minter)

	for _, path := range []string{"/onboard/mint", "/onboard/write"} {
		resp, err := noRedirectClient().Get(ts.URL + path + "?project=proj-11")
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Errorf("GET %s = %d, want 405", path, resp.StatusCode)
		}
	}
	if len(minter.minted) != 0 {
		t.Error("a GET must not mint a token")
	}
}

// The rail must show Connect as a real step in the flow.
func TestConnect_AppearsInTheRail(t *testing.T) {
	ts := connectFixture(t, &fakeMinter{})

	body := getBody(t, ts, "/onboard/connect?project=proj-11")
	if !strings.Contains(body, "Connect") {
		t.Error("Connect should be a step in the rail")
	}
	if !strings.Contains(body, `<li class="current">`) {
		t.Error("the connect step should be marked current")
	}
}
