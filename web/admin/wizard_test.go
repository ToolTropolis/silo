package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The bare /onboard step redirects into the wizard so a stale link still lands
// somewhere useful.
func TestWizard_RootRedirectsToFirstStep(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil)})

	resp, err := noRedirectClient().Get(ts.URL + "/onboard/")
	if err != nil {
		t.Fatalf("GET /onboard/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/onboard/name" {
		t.Errorf("Location = %q, want /onboard/name", loc)
	}
}

// Each step is its own URL, so it is linkable, refreshable, and back-button
// safe — none of which a single form with hidden state gives you.
func TestWizard_StepsAreIndividuallyAddressable(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: &fakeRegistry{},
		Settings: newFakeSettings(nil),
		Prov:     &fakeProvisioner{},
	})

	for _, path := range []string{
		"/onboard/name",
		"/onboard/name?project=newproj",
		"/onboard/checks?project=newproj",
		"/onboard/review?project=newproj",
	} {
		resp, err := noRedirectClient().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

// Validation shows inline on the naming step rather than after a round trip
// through provisioning.
func TestWizard_NameStepValidatesInline(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/onboard/name?project=Bad_ID")
	if !strings.Contains(body, "uppercase") {
		t.Error("an invalid ID should be explained on the naming step")
	}

	// A valid ID previews exactly what the name will become — both are
	// permanent, so seeing them before committing matters.
	body = getBody(t, ts, "/onboard/name?project=newproj")
	if !strings.Contains(body, "silo-newproj") {
		t.Error("a valid ID should preview its bucket name")
	}
	if !strings.Contains(body, "newproj.bbolt") {
		t.Error("a valid ID should preview its cache filename")
	}
}

// An empty form must not open with an error on it.
func TestWizard_NameStepStartsClean(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/onboard/name")
	if strings.Contains(body, "class=\"err\"") {
		t.Error("the naming step should not show an error before anything is typed")
	}
}

// A blocked preflight must disable the continue control, not merely warn.
func TestWizard_BlockedChecksDisableContinue(t *testing.T) {
	ts := newFixture(t, Config{
		Registry:   activeProject("taken"), // the ID is already registered
		Settings:   newFakeSettings(nil),
		Prov:       &fakeProvisioner{},
		CredsProbe: probeFunc(func(context.Context) error { return nil }),
	})

	body := getBody(t, ts, "/onboard/checks?project=taken")
	if !strings.Contains(body, "Onboarding would fail") {
		t.Error("a blocked preflight should say so")
	}
	if !strings.Contains(body, "disabled") {
		t.Error("continue must be disabled when preflight blocks")
	}
	if strings.Contains(body, `href="/onboard/review?project=taken`) {
		t.Error("a blocked preflight must not offer a link past the checks")
	}
}

func TestWizard_PassingChecksOfferContinue(t *testing.T) {
	ts := newFixture(t, Config{
		Registry:     &fakeRegistry{},
		Settings:     newFakeSettings(nil),
		Prov:         &fakeProvisioner{},
		BackendProbe: probeFunc(func(context.Context) error { return nil }),
		CredsProbe:   probeFunc(func(context.Context) error { return nil }),
	})

	body := getBody(t, ts, "/onboard/checks?project=newproj")
	if !strings.Contains(body, "Ready to provision") {
		t.Error("a clean preflight should say it is ready")
	}
	if !strings.Contains(body, `href="/onboard/review?project=newproj`) {
		t.Error("a clean preflight should link to review")
	}
}

// Review states exactly what will exist afterwards, so nothing is a surprise.
func TestWizard_ReviewListsEveryResource(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: &fakeRegistry{},
		Settings: newFakeSettings(nil),
		Prov:     &fakeProvisioner{},
	})

	body := getBody(t, ts, "/onboard/review?project=newproj")
	for _, want := range []string{"silo-newproj", "projects/newproj", "silo-cred-newproj", "newproj.bbolt"} {
		if !strings.Contains(body, want) {
			t.Errorf("review should name %q", want)
		}
	}
}

// Provisioning is a POST — a GET must not trigger it, so a prefetch or a
// refreshed URL cannot create a project.
func TestWizard_ProvisionRequiresPOST(t *testing.T) {
	fp := &fakeProvisioner{}
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil), Prov: fp})

	resp, err := noRedirectClient().Get(ts.URL + "/onboard/provision?project=newproj")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if len(fp.onboarded) != 0 {
		t.Error("a GET must not provision anything")
	}
}

func TestWizard_ProvisionRejectsInvalidID(t *testing.T) {
	fp := &fakeProvisioner{}
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil), Prov: fp})

	resp := postForm(t, ts, "/onboard/provision", url.Values{"project": {"Bad_ID"}})
	if _, errMsg := flashOf(t, resp); errMsg == "" {
		t.Error("an invalid ID must be rejected")
	}
	if len(fp.onboarded) != 0 {
		t.Error("an invalid ID reached the provisioner")
	}
}

// The happy path: provision, then poll status until it settles, and confirm
// every layer is reported as created.
func TestWizard_ProvisionReportsEveryLayer(t *testing.T) {
	fp := &fakeProvisioner{}
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil), Prov: fp})

	resp := postForm(t, ts, "/onboard/provision", url.Values{"project": {"newproj"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "/onboard/status") {
		t.Fatalf("Location = %q, want the status page", loc)
	}

	body := pollStatus(t, ts, "newproj")
	if !strings.Contains(body, "is ready") {
		t.Fatalf("provisioning did not report success:\n%s", firstLines(body, 40))
	}
	for _, layer := range []string{"Registry record", "Per-project encryption key",
		"Versioned bucket", "Scoped S3 credential"} {
		if !strings.Contains(body, layer) {
			t.Errorf("status should list the %q layer", layer)
		}
	}
}

// The failure path is the one that earns the wizard: it must name the layer
// that failed and say the rest were rolled back, rather than showing one opaque
// error string.
func TestWizard_FailureNamesTheLayerAndReportsRollback(t *testing.T) {
	fp := &fakeProvisioner{
		onboardErr: errors.New(`admin: onboard "newproj": issue credential: weed not found`),
	}
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil), Prov: fp})

	postForm(t, ts, "/onboard/provision", url.Values{"project": {"newproj"}})
	body := pollStatus(t, ts, "newproj")

	if !strings.Contains(body, "rolled back") {
		t.Error("a failure must state that the other layers were rolled back")
	}
	if !strings.Contains(body, "failed — rolled back") {
		t.Error("the failing layer should be marked distinctly from the rolled-back ones")
	}
	if !strings.Contains(body, "weed not found") {
		t.Error("the underlying error should still be shown")
	}
}

// Status for a project that was never provisioned sends the operator back to
// the start rather than rendering an empty progress list.
func TestWizard_StatusForUnknownProjectRedirects(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil), Prov: &fakeProvisioner{}})

	resp, err := noRedirectClient().Get(ts.URL + "/onboard/status?project=never-started")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want a redirect", resp.StatusCode)
	}
}

func TestWizard_DoneShowsConnectionInstructions(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil), Prov: &fakeProvisioner{}})

	body := getBody(t, ts, "/onboard/done?project=newproj")
	if !strings.Contains(body, "silod --tokens") {
		t.Error("the done step should show how to run a daemon for the project")
	}
	// The whole point of onboarding is wiring a repo to Silo, so the final step
	// must hand over real agent config rather than raw API calls.
	if !strings.Contains(body, "mcpServers") {
		t.Error("the done step should emit .mcp.json config for the agent runtime")
	}
	if !strings.Contains(body, "SILO_PROJECT") || !strings.Contains(body, "newproj") {
		t.Error("the emitted config should be scoped to this project")
	}
	// The token must be referenced by environment variable, never inlined —
	// .mcp.json is normally committed.
	if !strings.Contains(body, "${SILO_TOKEN}") {
		t.Error("the token must be an env reference so it is not committed")
	}
	for _, tool := range []string{"silo_read", "silo_write", "silo_list", "silo_search"} {
		if !strings.Contains(body, tool) {
			t.Errorf("the done step should name the %s tool", tool)
		}
	}
}

func TestWizard_UnknownStepIs404(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil)})

	resp, err := noRedirectClient().Get(ts.URL + "/onboard/nonsense")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// The rail marks completed steps distinctly from upcoming ones, so an operator
// can see where they are in the flow.
func TestWizard_RailTracksProgress(t *testing.T) {
	ts := newFixture(t, Config{
		Registry:   &fakeRegistry{},
		Settings:   newFakeSettings(nil),
		Prov:       &fakeProvisioner{},
		CredsProbe: probeFunc(func(context.Context) error { return nil }),
	})

	body := getBody(t, ts, "/onboard/review?project=newproj")
	if !strings.Contains(body, `<li class="done">`) {
		t.Error("earlier steps should be marked done")
	}
	if !strings.Contains(body, `<li class="current">`) {
		t.Error("the active step should be marked current")
	}
	if !strings.Contains(body, `<li class="todo">`) {
		t.Error("later steps should be marked todo")
	}
}

// --- helpers ---------------------------------------------------------------

// pollStatus waits for background provisioning to settle. Bounded so a stuck
// provision fails the test rather than hanging it.
func pollStatus(t *testing.T, ts *httptest.Server, project string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		body = getBody(t, ts, "/onboard/status?project="+project)
		if strings.Contains(body, "is ready") || strings.Contains(body, "rolled back") {
			return body
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("provisioning never settled for %q", project)
	return body
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// Step 1 asks for the repository, because that is what an operator is thinking
// about. The project ID is derived from it.
func TestWizard_DerivesProjectIDFromARepoURL(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/onboard/name?repo=https://github.com/org/MyService.git")

	if !strings.Contains(body, `value="myservice"`) {
		t.Error("the project ID should be prefilled from the repo name")
	}
	if !strings.Contains(body, "derived from your repo") {
		t.Error("an autofilled ID should be labelled as derived")
	}
}

// A name that had to be changed must say so: the result becomes a bucket name,
// and an operator who does not notice will look for a bucket that is not there.
func TestWizard_ShowsWhenTheNameWasNormalized(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/onboard/name?repo=https://github.com/org/My_Service.v2.git")
	if !strings.Contains(body, "Adjusted") {
		t.Error("a normalized name should be reported")
	}
	if !strings.Contains(body, "my-service-v2") {
		t.Errorf("the normalized ID should be shown")
	}
}

// A name needing no change must not be reported as adjusted.
func TestWizard_UnchangedNameIsNotReportedAsAdjusted(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/onboard/name?repo=https://github.com/org/my-service.git")
	if strings.Contains(body, "Adjusted") {
		t.Error("nothing changed, so nothing should be reported as adjusted")
	}
	if !strings.Contains(body, `value="my-service"`) {
		t.Error("the ID should still be prefilled")
	}
}

// The derived ID is a suggestion, not a decision: an operator may be onboarding
// a second project for one repo, or keeping an existing ID.
func TestWizard_ExplicitProjectIDWinsOverTheDerivedOne(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/onboard/name?repo=https://github.com/org/myrepo.git&project=chosen-name")
	if !strings.Contains(body, `value="chosen-name"`) {
		t.Error("an explicitly supplied ID must not be overwritten by derivation")
	}
}

// A bad repo path is explained on the step rather than silently ignored.
func TestWizard_BadRepoInputIsExplained(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/onboard/name?repo=relative/path")
	if !strings.Contains(body, "absolute") {
		t.Error("a bad repo path should be explained")
	}
}

// Naming a project without a repo must still work — the repo is a convenience,
// not a requirement.
func TestWizard_RepoIsOptional(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/onboard/name?project=manual-name")
	if !strings.Contains(body, `value="manual-name"`) {
		t.Error("naming a project directly should still work")
	}
	if strings.Contains(body, "derived from your repo") {
		t.Error("nothing was derived, so nothing should claim to be")
	}
}

// The repo has to survive to the Connect step, or the operator would type it
// twice.
func TestWizard_RepoIsCarriedThroughTheFlow(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: &fakeRegistry{}, Settings: newFakeSettings(nil),
		Prov: &fakeProvisioner{}, CredsProbe: probeFunc(func(context.Context) error { return nil }),
	})
	repo := "https://github.com/org/myrepo.git"

	body := getBody(t, ts, "/onboard/checks?project=myrepo&repo="+url.QueryEscape(repo))
	if !strings.Contains(body, "repo=https") {
		t.Error("the checks step should carry the repo forward")
	}

	body = getBody(t, ts, "/onboard/review?project=myrepo&repo="+url.QueryEscape(repo))
	if !strings.Contains(body, `name="repo"`) {
		t.Error("the review step should carry the repo into provisioning")
	}
}
