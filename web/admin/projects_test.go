package admin

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestOnboard_ValidIDIsProvisioned(t *testing.T) {
	fp := &fakeProvisioner{}
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Prov: fp})

	resp := postForm(t, ts, "/onboard", url.Values{"project": {"newproj"}})
	flash, errMsg := flashOf(t, resp)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if !strings.Contains(flash, "onboarded") {
		t.Errorf("flash = %q, want an onboarding confirmation", flash)
	}
	if len(fp.onboarded) != 1 || fp.onboarded[0] != "newproj" {
		t.Errorf("onboarded = %v, want [newproj]", fp.onboarded)
	}
}

// Validation happens before anything is provisioned: onboarding bakes the ID
// into a bucket name and a cache filename permanently, so a bad ID must never
// reach the provisioner at all.
func TestOnboard_InvalidIDNeverReachesTheProvisioner(t *testing.T) {
	bad := []string{
		"",           // empty
		"ab",         // too short
		"Repo1",      // uppercase would let two IDs share one bucket
		"my_project", // underscore is not a legal bucket character
		"my.project", // dots break bucket naming
		"../escape",  // traversal
		"proj/sub",   // path separator
		strings.Repeat("a", 200),
	}
	for _, id := range bad {
		fp := &fakeProvisioner{}
		ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Prov: fp})

		resp := postForm(t, ts, "/onboard", url.Values{"project": {id}})
		if _, errMsg := flashOf(t, resp); errMsg == "" {
			t.Errorf("onboarding %q should be rejected", id)
		}
		if len(fp.onboarded) != 0 {
			t.Errorf("%q reached the provisioner despite being invalid", id)
		}
	}
}

// Whitespace is trimmed rather than making a pasted ID fail validation for a
// reason the operator cannot see.
func TestOnboard_TrimsWhitespace(t *testing.T) {
	fp := &fakeProvisioner{}
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Prov: fp})

	postForm(t, ts, "/onboard", url.Values{"project": {"  newproj \n"}})
	if len(fp.onboarded) != 1 || fp.onboarded[0] != "newproj" {
		t.Errorf("onboarded = %v, want [newproj] with whitespace trimmed", fp.onboarded)
	}
}

func TestOnboard_FailureIsReported(t *testing.T) {
	fp := &fakeProvisioner{onboardErr: errors.New("issue credential: weed not found")}
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Prov: fp})

	resp := postForm(t, ts, "/onboard", url.Values{"project": {"newproj"}})
	_, errMsg := flashOf(t, resp)
	if !strings.Contains(errMsg, "weed not found") {
		t.Errorf("error = %q, want the provisioning failure surfaced", errMsg)
	}
}

// The irreversible step requires the project ID typed exactly. A misplaced
// click must never destroy a bucket.
func TestTeardown_DeleteBucketRequiresTypedConfirmation(t *testing.T) {
	for _, confirm := range []string{"", "y", "yes", "proj-1", "PROJ-11", " proj-11x"} {
		fp := &fakeProvisioner{}
		ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Prov: fp})

		resp := postForm(t, ts, "/teardown", url.Values{
			"project": {"proj-11"},
			"step":    {"delete-bucket"},
			"confirm": {confirm},
		})
		if _, errMsg := flashOf(t, resp); errMsg == "" {
			t.Errorf("confirm=%q should be refused", confirm)
		}
		if len(fp.steps) != 0 {
			t.Errorf("confirm=%q reached the provisioner: %v", confirm, fp.steps)
		}
	}
}

func TestTeardown_DeleteBucketProceedsOnExactMatch(t *testing.T) {
	fp := &fakeProvisioner{}
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Prov: fp})

	resp := postForm(t, ts, "/teardown", url.Values{
		"project": {"proj-11"},
		"step":    {"delete-bucket"},
		"confirm": {"proj-11"},
	})
	if _, errMsg := flashOf(t, resp); errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if len(fp.steps) != 1 || fp.steps[0] != "proj-11:delete-bucket" {
		t.Errorf("steps = %v, want the delete-bucket step to run", fp.steps)
	}
}

// The reversible steps do not need a typed ID — the browser confirm is enough,
// matching siloctl's own y/N prompt.
func TestTeardown_ReversibleStepsNeedNoTypedID(t *testing.T) {
	for _, step := range []string{"revoke-credential", "revoke-key", "deregister"} {
		fp := &fakeProvisioner{}
		ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Prov: fp})

		resp := postForm(t, ts, "/teardown", url.Values{
			"project": {"proj-11"},
			"step":    {step},
		})
		if _, errMsg := flashOf(t, resp); errMsg != "" {
			t.Errorf("%s: unexpected error %s", step, errMsg)
		}
		if len(fp.steps) != 1 {
			t.Errorf("%s did not run: %v", step, fp.steps)
		}
	}
}

func TestTeardown_RequiresProjectAndStep(t *testing.T) {
	fp := &fakeProvisioner{}
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Prov: fp})

	for _, form := range []url.Values{
		{"step": {"deregister"}},
		{"project": {"proj-11"}},
		{},
	} {
		resp := postForm(t, ts, "/teardown", form)
		if _, errMsg := flashOf(t, resp); errMsg == "" {
			t.Errorf("form %v should be rejected", form)
		}
	}
	if len(fp.steps) != 0 {
		t.Errorf("nothing should have run: %v", fp.steps)
	}
}

// An ordering violation from internal/admin must reach the operator intact:
// they need to know which step is actually next.
func TestTeardown_OrderingErrorIsSurfaced(t *testing.T) {
	fp := &fakeProvisioner{teardownErr: errors.New("not ready for \"deregister\"; next is \"delete-bucket\"")}
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Prov: fp})

	resp := postForm(t, ts, "/teardown", url.Values{
		"project": {"proj-11"},
		"step":    {"deregister"},
	})
	_, errMsg := flashOf(t, resp)
	if !strings.Contains(errMsg, "next is") {
		t.Errorf("error = %q, want the ordering guidance preserved", errMsg)
	}
}

// Without a provisioner the page must not offer actions it cannot perform.
func TestProjects_NoProvisionerDisablesActions(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/projects")
	if !strings.Contains(body, "No provisioner configured") {
		t.Error("the page should say provisioning is unavailable")
	}
	if strings.Contains(body, `action="/onboard"`) {
		t.Error("the onboard form must not render without a provisioner")
	}
}

// Unsynced writes are shown next to the teardown control: tearing down a
// project that has them loses those writes.
func TestProjects_ShowsUnsyncedNextToTeardown(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
		Prov:     &fakeProvisioner{plan: []TeardownStep{{Name: "revoke-credential"}}},
		Daemon:   &fakeDaemon{stats: []ProjectCacheStat{{Project: "proj-11", Pending: 3}}},
	})

	body := getBody(t, ts, "/projects")
	if !strings.Contains(body, `class="unsynced">3<`) {
		t.Error("an unsynced count should be highlighted on the projects view")
	}
}

// With no daemon the count is unknown, not zero — the same rule as the cache
// view, and it matters more here because teardown is destructive.
func TestProjects_UnknownUnsyncedWhenNoDaemon(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
		Prov:     &fakeProvisioner{},
	})

	body := getBody(t, ts, "/projects")
	if !strings.Contains(body, "unknown") {
		t.Error("unsynced counts must render as unknown when no daemon is reachable")
	}
}
