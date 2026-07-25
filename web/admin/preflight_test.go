package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// failingProbe reports a fixed error, standing in for an unreachable dependency.
type failingProbe struct{ err error }

func (f failingProbe) Probe(context.Context) error { return f.err }

func statusOf(r PreflightReport, name string) (PreflightCheck, bool) {
	for _, c := range r.Checks {
		if strings.Contains(c.Name, name) {
			return c, true
		}
	}
	return PreflightCheck{}, false
}

// The point of preflight: a dependency that would fail at layer four is caught
// before layer one is created, so the operator never has to reason about a
// rollback.
func TestPreflight_BlocksOnBrokenCredentialIssuer(t *testing.T) {
	pf := &Preflighter{
		Registry: &fakeRegistry{},
		Creds:    failingProbe{err: errors.New("weed: executable file not found in $PATH")},
	}
	report := pf.Run(context.Background(), "newproj")

	if !report.Blocked {
		t.Fatal("a broken credential issuer must block onboarding")
	}
	c, ok := statusOf(report, "Credential issuer")
	if !ok {
		t.Fatal("no credential check ran")
	}
	if c.Status != CheckFail {
		t.Errorf("status = %q, want fail", c.Status)
	}
	// The fix text is the whole value of the check: an operator who sees only
	// "failed" has learned nothing they could not have learned by trying.
	if !strings.Contains(c.Fix, "seaweedfs") {
		t.Errorf("Fix = %q, want installation guidance", c.Fix)
	}
}

func TestPreflight_BlocksInvalidID(t *testing.T) {
	pf := &Preflighter{Registry: &fakeRegistry{}}
	for _, id := range []string{"", "ab", "Repo1", "my_project", "../escape"} {
		report := pf.Run(context.Background(), id)
		if !report.Blocked {
			t.Errorf("%q should block onboarding", id)
		}
	}
}

func TestPreflight_BlocksTakenID(t *testing.T) {
	pf := &Preflighter{Registry: activeProject("taken")}
	report := pf.Run(context.Background(), "taken")

	if !report.Blocked {
		t.Fatal("an already-registered ID must block")
	}
	c, _ := statusOf(report, "available")
	if c.Status != CheckFail {
		t.Errorf("availability = %q, want fail", c.Status)
	}
}

func TestPreflight_PassesForAFreshProject(t *testing.T) {
	pf := &Preflighter{
		Registry: &fakeRegistry{},
		Daemon:   &fakeDaemon{},
		Backend:  probeFunc(func(context.Context) error { return nil }),
		Creds:    probeFunc(func(context.Context) error { return nil }),
	}
	report := pf.Run(context.Background(), "newproj")

	if report.Blocked {
		t.Fatalf("a clean environment should not block: %+v", report.Blockers())
	}
	for _, c := range report.Checks {
		if c.Status != CheckPass {
			t.Errorf("check %q = %q, want pass", c.Name, c.Status)
		}
	}
}

// An unconfigured dependency must warn, never pass. "Healthy" and "unchecked"
// look identical to an operator otherwise, on a page whose entire job is to say
// whether provisioning will work.
func TestPreflight_UnconfiguredDependenciesWarnRatherThanPass(t *testing.T) {
	pf := &Preflighter{Registry: &fakeRegistry{}} // no daemon, backend, or creds
	report := pf.Run(context.Background(), "newproj")

	if report.Blocked {
		t.Error("an unverifiable dependency should warn, not block")
	}
	for _, name := range []string{"Credential issuer", "Storage backend", "leftover local cache"} {
		c, ok := statusOf(report, name)
		if !ok {
			t.Errorf("check %q did not run", name)
			continue
		}
		if c.Status != CheckWarn {
			t.Errorf("check %q = %q, want warn when not configured", name, c.Status)
		}
	}
}

// A missing registry blocks: without it nothing can be registered at all.
func TestPreflight_MissingRegistryBlocks(t *testing.T) {
	pf := &Preflighter{}
	report := pf.Run(context.Background(), "newproj")

	if !report.Blocked {
		t.Fatal("no registry must block onboarding")
	}
}

// A leftover cache warns rather than blocks: the generation stamp means the new
// project cannot read the old memory, so this is a hygiene signal, not a hazard.
func TestPreflight_LeftoverCacheWarnsButDoesNotBlock(t *testing.T) {
	pf := &Preflighter{
		Registry: &fakeRegistry{},
		Daemon: &fakeDaemon{stats: []ProjectCacheStat{
			{Project: "reused", Entries: 12, FileBytes: 131072},
		}},
		Backend: probeFunc(func(context.Context) error { return nil }),
		Creds:   probeFunc(func(context.Context) error { return nil }),
	}
	report := pf.Run(context.Background(), "reused")

	if report.Blocked {
		t.Error("a leftover cache must not block; the generation stamp makes it safe")
	}
	c, _ := statusOf(report, "leftover local cache")
	if c.Status != CheckWarn {
		t.Errorf("status = %q, want warn", c.Status)
	}
	if !strings.Contains(c.Fix, "generation") {
		t.Errorf("Fix = %q, want it to explain why proceeding is safe", c.Fix)
	}
}

// A registry that errors mid-check must not be mistaken for an available ID.
func TestPreflight_RegistryErrorDoesNotLookAvailable(t *testing.T) {
	pf := &Preflighter{Registry: &fakeRegistry{err: errors.New("rqlite unreachable")}}
	report := pf.Run(context.Background(), "newproj")

	if !report.Blocked {
		t.Error("an unreachable registry must block")
	}
	c, _ := statusOf(report, "Registry is reachable")
	if c.Status != CheckFail {
		t.Errorf("reachability = %q, want fail", c.Status)
	}
}

func TestPreflightReport_Blockers(t *testing.T) {
	r := PreflightReport{Checks: []PreflightCheck{
		{Name: "a", Status: CheckPass},
		{Name: "b", Status: CheckWarn},
		{Name: "c", Status: CheckFail},
		{Name: "d", Status: CheckFail},
	}}
	if got := len(r.Blockers()); got != 2 {
		t.Errorf("Blockers() = %d, want only the 2 failures", got)
	}
}
