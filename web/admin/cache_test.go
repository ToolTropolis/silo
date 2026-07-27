package admin

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tooltropolis/silo/internal/registry"
)

// Per-field precedence, rendered with attribution: an operator who cannot tell
// an override from an inherited default cannot debug a policy.
func TestCacheView_ShowsPerFieldPrecedence(t *testing.T) {
	fs := newFakeSettings(map[string]registry.CacheSettings{
		registry.FleetKey: {TTL: ttl(720 * time.Hour), MaxBytes: bytes64(512 << 20)},
		"proj-11":         {MaxEntries: entries(250)},
	})
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: fs,
		Daemon: &fakeDaemon{stats: []ProjectCacheStat{
			{Project: "proj-11", Entries: 300, Bytes: 16000, FileBytes: 262144},
		}},
	})

	body := getBody(t, ts, "/")
	for _, want := range []string{
		`TTL: <span class="mono">720h0m0s</span><span class="src fleet">`,
		`Max entries: <span class="mono">250</span><span class="src project">`,
		`Max bytes: <span class="mono">512.0 MiB</span><span class="src fleet">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in the policy column", want)
		}
	}
}

// A field set at neither level is attributed to the daemon, since the console
// cannot see per-host flags and must not invent a value for them.
func TestCacheView_UnsetFieldsAttributedToTheDaemon(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
		Daemon:   &fakeDaemon{stats: []ProjectCacheStat{{Project: "proj-11"}}},
	})

	body := getBody(t, ts, "/")
	if !strings.Contains(body, `TTL: <span class="mono">daemon default</span><span class="src daemon">`) {
		t.Error("an unset field should be attributed to the daemon, not shown as a value")
	}
}

// The property that matters most on this page: "nothing cached" and "nobody
// checked" must not look the same. A reassuring zero on an unreachable daemon
// would tell an operator their cache is empty when it may be full.
func TestCacheView_UnreachableDaemonShowsUnknownNotZero(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
		Daemon:   &fakeDaemon{statsErr: errors.New("connection refused")},
	})

	body := getBody(t, ts, "/")
	if !strings.Contains(body, "unknown") {
		t.Error("an unreachable daemon must render as unknown")
	}
	if !strings.Contains(body, "connection refused") {
		t.Error("the reason the daemon is unreachable should be shown")
	}
}

// A per-project stats failure is reported for that project alone, not as an
// empty row.
func TestCacheView_PerProjectStatsErrorIsShown(t *testing.T) {
	ts := newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
		Daemon: &fakeDaemon{stats: []ProjectCacheStat{
			{Project: "proj-11", StatsError: "cache file locked"},
		}},
	})

	body := getBody(t, ts, "/")
	if !strings.Contains(body, "cache file locked") {
		t.Error("a per-project stats error should be surfaced on its row")
	}
}

// With no daemon at all the page still renders policy — the half that comes
// from the registry is still accurate and actionable.
func TestCacheView_NoDaemonStillRendersPolicy(t *testing.T) {
	fs := newFakeSettings(map[string]registry.CacheSettings{
		"proj-11": {MaxEntries: entries(42)},
	})
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: fs})

	body := getBody(t, ts, "/")
	if !strings.Contains(body, `Max entries: <span class="mono">42</span>`) {
		t.Error("policy should render even with no daemon configured")
	}
	if !strings.Contains(body, "no daemon configured") {
		t.Error("the missing daemon should be stated plainly")
	}
}

func TestCacheAction_Compact(t *testing.T) {
	fd := &fakeDaemon{compact: CompactOutcome{
		Compacted: true, Reclaimed: 16 << 20, BytesBefore: 32 << 20, BytesAfter: 16 << 20,
	}}
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Daemon: fd})

	resp := postForm(t, ts, "/cache-action", url.Values{
		"project": {"proj-11"}, "action": {"compact"},
	})
	flash, errMsg := flashOf(t, resp)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if !strings.Contains(flash, "16.0 MiB") {
		t.Errorf("flash = %q, want the reclaimed size reported", flash)
	}
	if len(fd.compacted) != 1 || fd.compacted[0] != "proj-11" {
		t.Errorf("compacted = %v, want [proj-11]", fd.compacted)
	}
}

// A skip is the safety gate working, so it is reported as information rather
// than as a failure the operator needs to chase.
func TestCacheAction_CompactSkipIsInformational(t *testing.T) {
	fd := &fakeDaemon{compact: CompactOutcome{Compacted: false, SkipReason: "1 write(s) queued"}}
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Daemon: fd})

	resp := postForm(t, ts, "/cache-action", url.Values{
		"project": {"proj-11"}, "action": {"compact"},
	})
	flash, errMsg := flashOf(t, resp)
	if errMsg != "" {
		t.Errorf("a safe skip should not be an error, got %q", errMsg)
	}
	if !strings.Contains(flash, "1 write(s) queued") {
		t.Errorf("flash = %q, want the skip reason", flash)
	}
}

// A refused purge IS an error to the operator: they asked for something that
// did not happen, and the reason determines what they do next.
func TestCacheAction_PurgeRefusalIsReported(t *testing.T) {
	fd := &fakeDaemon{purge: PurgeOutcome{Purged: false, Pending: 3, Reason: "3 write(s) still queued"}}
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Daemon: fd})

	resp := postForm(t, ts, "/cache-action", url.Values{
		"project": {"proj-11"}, "action": {"purge"},
	})
	_, errMsg := flashOf(t, resp)
	if !strings.Contains(errMsg, "3 write(s) still queued") {
		t.Errorf("error = %q, want the refusal reason", errMsg)
	}
}

func TestCacheAction_PurgeSuccess(t *testing.T) {
	fd := &fakeDaemon{purge: PurgeOutcome{Purged: true}}
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Daemon: fd})

	resp := postForm(t, ts, "/cache-action", url.Values{
		"project": {"proj-11"}, "action": {"purge"},
	})
	flash, errMsg := flashOf(t, resp)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if !strings.Contains(flash, "purged") {
		t.Errorf("flash = %q, want a purge confirmation", flash)
	}
}

func TestCacheAction_RejectsBadRequests(t *testing.T) {
	fd := &fakeDaemon{}
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil), Daemon: fd})

	// Unknown action.
	resp := postForm(t, ts, "/cache-action", url.Values{
		"project": {"proj-11"}, "action": {"delete-everything"},
	})
	if _, errMsg := flashOf(t, resp); errMsg == "" {
		t.Error("an unknown action must be rejected")
	}

	// No project.
	resp = postForm(t, ts, "/cache-action", url.Values{"action": {"purge"}})
	if _, errMsg := flashOf(t, resp); errMsg == "" {
		t.Error("a missing project must be rejected")
	}

	if len(fd.purged) != 0 || len(fd.compacted) != 0 {
		t.Errorf("no action should have reached the daemon: purged=%v compacted=%v",
			fd.purged, fd.compacted)
	}
}

func TestReclaimable(t *testing.T) {
	tests := []struct {
		name string
		stat ProjectCacheStat
		want int64
	}{
		{"a bloated file reports the gap", ProjectCacheStat{Bytes: 1000, FileBytes: 5000}, 4000},
		{"a dense file reclaims nothing", ProjectCacheStat{Bytes: 5000, FileBytes: 5000}, 0},
		// Content larger than the file cannot happen, but reporting a negative
		// reclaim would render as nonsense rather than as the zero it means.
		{"never negative", ProjectCacheStat{Bytes: 9000, FileBytes: 5000}, 0},
		{"empty", ProjectCacheStat{}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stat.Reclaimable(); got != tc.want {
				t.Errorf("Reclaimable() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"},
		{16 << 20, "16.0 MiB"},
		{1 << 30, "1.0 GiB"},
		{1 << 40, "1.0 TiB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Zero is a real policy here (never cache), so it must not render as an
// absence.
func TestHumanDuration_ZeroIsExplicit(t *testing.T) {
	if got := humanDuration(0); !strings.Contains(got, "never") {
		t.Errorf("humanDuration(0) = %q, want it to state that nothing is cached", got)
	}
	if got := humanDuration(time.Hour); got != "1h0m0s" {
		t.Errorf("humanDuration(1h) = %q", got)
	}
}
