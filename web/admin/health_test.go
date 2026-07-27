package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A dead daemon means stale cache numbers and failing maintenance actions.
// Without a header indicator that only surfaces as a confusing error on
// whichever page happened to need it.
func TestHeader_ShowsDaemonUp(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
		Daemon:   &fakeDaemon{stats: []ProjectCacheStat{{Project: "proj-11"}}},
	})

	body := getBody(t, ts, "/projects")
	if !strings.Contains(body, "daemon up") {
		t.Error("a reachable daemon should show as up in the header")
	}
}

func TestHeader_ShowsDaemonDown(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
		Daemon:   &fakeDaemon{statsErr: errors.New("connection refused")},
	})

	body := getBody(t, ts, "/projects")
	if !strings.Contains(body, "daemon down") {
		t.Error("an unreachable daemon should show as down")
	}
	if !strings.Contains(body, "connection refused") {
		t.Error("the reason should be in the tooltip")
	}
}

// "not configured" and "down" are different problems with different fixes.
func TestHeader_NotConfiguredIsNotDown(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/projects")
	if !strings.Contains(body, "no daemon") {
		t.Error("an unconfigured daemon should say so")
	}
	if strings.Contains(body, "daemon down") {
		t.Error("unconfigured must not be reported as down")
	}
}

// The probe is cached: every page render must not hit the daemon.
func TestHealth_IsCached(t *testing.T) {
	fd := &countingDaemon{}
	// Probe health() directly: rendering a page also calls CacheStats for its
	// own data, which would be counted here and hide whether the cache works.
	srv, err := NewServer(Config{
		Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Daemon: fd,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	for range 5 {
		srv.health()
	}
	if fd.calls != 1 {
		t.Errorf("probed %d times over 5 calls, want 1 within the cache TTL", fd.calls)
	}
}

// The dashboard already browses memory; the console links rather than rebuilds.
func TestProject_LinksToDashboardWhenConfigured(t *testing.T) {
	ts := newFixture(t, Config{
		Registry:     activeProject("proj-11"),
		Settings:     newFakeSettings(nil),
		DashboardURL: "http://127.0.0.1:8600/",
	})

	body := getBody(t, ts, "/project?project=proj-11")
	if !strings.Contains(body, "http://127.0.0.1:8600/memory?project=proj-11") {
		t.Error("the project page should link to the dashboard's memory browser")
	}
	// The trailing slash must not produce a double slash in the link.
	if strings.Contains(body, "8600//memory") {
		t.Error("trailing slash was not trimmed")
	}
}

func TestProject_NoDashboardLinkWhenUnset(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/project?project=proj-11")
	if strings.Contains(body, "Browse this project's memory") {
		t.Error("no dashboard configured, so no link should render")
	}
}

// countingDaemon counts probes.
type countingDaemon struct{ calls int }

func (c *countingDaemon) CacheStats(context.Context) ([]ProjectCacheStat, error) {
	c.calls++
	return nil, nil
}
func (c *countingDaemon) PurgeCache(context.Context, string) (PurgeOutcome, error) {
	return PurgeOutcome{}, nil
}
func (c *countingDaemon) CacheEntries(context.Context, string) ([]CacheEntry, error) {
	return nil, nil
}
func (c *countingDaemon) CompactCache(context.Context, string) (CompactOutcome, error) {
	return CompactOutcome{}, nil
}
