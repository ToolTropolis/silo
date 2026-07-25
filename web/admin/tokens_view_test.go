package admin

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tooltropolis/silo/internal/registry"
)

func projectFixture(t *testing.T, minter TokenMinter) *httptest.Server {
	t.Helper()
	return newFixture(t, Config{
		Registry:        activeProject("proj-11"),
		Settings:        newFakeSettings(nil),
		Prov:            &fakeProvisioner{},
		Tokens:          minter,
		AgentDaemonAddr: "http://127.0.0.1:8500",
		MCPBinary:       "silo-mcp",
	})
}

// The page must show enough to decide which token to revoke, and nothing that
// could be used as a credential.
func TestProjectPage_ListsTokensWithoutRevealingThem(t *testing.T) {
	minter := &fakeMinter{tokens: []registry.AgentToken{
		{Hash: "aaaaaaaaaaaabbbbbbbbcccc", ProjectID: "proj-11", Label: "laptop",
			CreatedAt: "2026-07-25T12:00:00Z", CreatedBy: "nav", LastUsedAt: "2026-07-25T13:00:00Z"},
		{Hash: "ddddddddddddeeeeeeeeffff", ProjectID: "proj-11", Label: "ci",
			CreatedAt: "2026-07-24T09:00:00Z", RevokedAt: "2026-07-25T10:00:00Z"},
	}}
	ts := projectFixture(t, minter)

	body := getBody(t, ts, "/project?project=proj-11")

	for _, want := range []string{"laptop", "ci", "aaaaaaaaaaaa", "revoked", "live"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page should show %q", want)
		}
	}
	// A token value must never appear on a listing page. The full hash does
	// appear, in the revoke form's hidden field — that is deliberate and safe:
	// a hash cannot authorize anything, and the form has to name which token to
	// revoke. What matters is that the credential itself is absent.
	if strings.Contains(body, "silo_pat_") {
		t.Error("no token value may appear on a listing page")
	}
	// The visible cell shows a prefix, so the table stays readable.
	if !strings.Contains(body, "aaaaaaaaaaaa…") {
		t.Error("the table should display a truncated hash, not the full one")
	}
}

// "Never used" is worth seeing: it is either a token nobody wired up, or one
// that can be revoked with no disruption.
func TestProjectPage_ShowsNeverUsedTokens(t *testing.T) {
	minter := &fakeMinter{tokens: []registry.AgentToken{
		{Hash: "abcdefabcdef0000", ProjectID: "proj-11", Label: "unused", CreatedAt: "2026-07-25T12:00:00Z"},
	}}
	ts := projectFixture(t, minter)

	body := getBody(t, ts, "/project?project=proj-11")
	if !strings.Contains(body, "never used") {
		t.Error("a token with no last-used timestamp should be marked")
	}
}

// Zero live tokens means no agent can reach the project — worth surfacing,
// since it looks like a broken integration rather than a configuration choice.
func TestProjectPage_HighlightsNoLiveTokens(t *testing.T) {
	minter := &fakeMinter{tokens: []registry.AgentToken{
		{Hash: "aaaa0000", ProjectID: "proj-11", RevokedAt: "2026-07-25T10:00:00Z"},
	}}
	ts := projectFixture(t, minter)

	body := getBody(t, ts, "/project?project=proj-11")
	if !strings.Contains(body, "no agent can reach this project") {
		t.Error("a project with no live token should say so")
	}
}

func TestProjectPage_EmptyStateInvitesMinting(t *testing.T) {
	ts := projectFixture(t, &fakeMinter{})

	body := getBody(t, ts, "/project?project=proj-11")
	if !strings.Contains(body, "No tokens yet") {
		t.Error("a project with no tokens should say so")
	}
	if !strings.Contains(body, "Mint token") {
		t.Error("the mint control should be offered")
	}
}

func TestProjectPage_UnknownProjectRedirects(t *testing.T) {
	ts := projectFixture(t, &fakeMinter{})

	resp, err := noRedirectClient().Get(ts.URL + "/project?project=nonexistent")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Errorf("status = %d, want a redirect", resp.StatusCode)
	}
	if _, errMsg := flashOf(t, resp); !strings.Contains(errMsg, "no such project") {
		t.Errorf("error = %q, want it to name the problem", errMsg)
	}
}

func TestProjectPage_MintRevealsOnce(t *testing.T) {
	minter := &fakeMinter{next: "silo_pat_PAGESECRET"}
	ts := projectFixture(t, minter)

	resp := postForm(t, ts, "/tokens/mint", url.Values{
		"project": {"proj-11"}, "label": {"laptop"},
	})
	// The token must never travel in a URL.
	if strings.Contains(resp.Header.Get("Location"), "PAGESECRET") {
		t.Fatal("the token leaked into the redirect URL")
	}

	body := getBody(t, ts, "/project?project=proj-11")
	if !strings.Contains(body, "silo_pat_PAGESECRET") {
		t.Error("the freshly minted token should be revealed")
	}
	if !strings.Contains(body, "Copy this token now") {
		t.Error("the reveal should be unmissable")
	}
}

func TestTokens_Revoke(t *testing.T) {
	minter := &fakeMinter{tokens: []registry.AgentToken{
		{Hash: "aaaaaaaaaaaa1111", ProjectID: "proj-11", Label: "laptop"},
	}}
	ts := projectFixture(t, minter)

	resp := postForm(t, ts, "/tokens/revoke", url.Values{
		"project": {"proj-11"}, "hash": {"aaaaaaaaaaaa1111"},
	})
	flash, errMsg := flashOf(t, resp)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if len(minter.revoked) != 1 || minter.revoked[0] != "aaaaaaaaaaaa1111" {
		t.Errorf("revoked = %v, want the named token", minter.revoked)
	}
	// Daemons cache verified tokens, so revocation is not instant everywhere.
	// Saying so prevents an operator assuming the token is dead immediately.
	if !strings.Contains(flash, "TTL") {
		t.Errorf("flash = %q, want it to mention the cache delay", flash)
	}
}

// The hash arrives from a form. Revoking another project's token because a
// field was tampered with would be a cross-project action from a per-project
// page — exactly the boundary this system exists to hold.
func TestTokens_CannotRevokeAnotherProjectsToken(t *testing.T) {
	minter := &fakeMinter{tokens: []registry.AgentToken{
		{Hash: "mine1111", ProjectID: "proj-11"},
	}}
	ts := projectFixture(t, minter)

	resp := postForm(t, ts, "/tokens/revoke", url.Values{
		"project": {"proj-11"}, "hash": {"someone-elses-token-hash"},
	})
	if _, errMsg := flashOf(t, resp); !strings.Contains(errMsg, "does not belong") {
		t.Errorf("error = %q, want the cross-project revoke refused", errMsg)
	}
	if len(minter.revoked) != 0 {
		t.Errorf("nothing should have been revoked, got %v", minter.revoked)
	}
}

func TestTokens_RevokeRequiresProjectAndHash(t *testing.T) {
	minter := &fakeMinter{}
	ts := projectFixture(t, minter)

	for _, form := range []url.Values{
		{"project": {"proj-11"}},
		{"hash": {"abc"}},
		{},
	} {
		resp := postForm(t, ts, "/tokens/revoke", form)
		if _, errMsg := flashOf(t, resp); errMsg == "" {
			t.Errorf("form %v should be rejected", form)
		}
	}
	if len(minter.revoked) != 0 {
		t.Error("nothing should have been revoked")
	}
}

// Mutating routes must reject GET, so a prefetch cannot mint or revoke.
func TestTokens_MutationsRejectGET(t *testing.T) {
	minter := &fakeMinter{}
	ts := projectFixture(t, minter)

	for _, path := range []string{"/tokens/mint", "/tokens/revoke"} {
		resp, err := noRedirectClient().Get(ts.URL + path + "?project=proj-11&hash=x")
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Errorf("GET %s = %d, want 405", path, resp.StatusCode)
		}
	}
	if len(minter.minted) != 0 || len(minter.revoked) != 0 {
		t.Error("a GET must not mint or revoke")
	}
}

// Without a token store the page still renders the project, and says why token
// management is unavailable.
func TestProjectPage_NoMinterDegradesGracefully(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
	})

	body := getBody(t, ts, "/project?project=proj-11")
	if !strings.Contains(body, "proj-11") {
		t.Error("the project should still render")
	}
	if strings.Contains(body, "Mint token") {
		t.Error("the mint control must not be offered without a token store")
	}
}

// The projects list links to each project's page, or token management would be
// unreachable.
func TestProjectsList_LinksToTheProjectPage(t *testing.T) {
	ts := projectFixture(t, &fakeMinter{})

	body := getBody(t, ts, "/projects")
	if !strings.Contains(body, `href="/project?project=proj-11"`) {
		t.Error("the projects list should link to each project's page")
	}
}
