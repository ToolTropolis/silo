package registry

import (
	"testing"
	"time"

	"github.com/tooltropolis/silo/internal/project"
)

func ttl(d time.Duration) *time.Duration { return &d }
func entries(n int) *int                 { return &n }
func bytes64(n int64) *int64             { return &n }

// Resolve is where the whole nullable-column design pays off, so exercise the
// precedence rules directly.
func TestResolve(t *testing.T) {
	tests := []struct {
		name           string
		project, fleet CacheSettings
		flags          CacheSettings
		wantTTL        *time.Duration
		wantEntries    *int
	}{
		{
			name:        "nothing set anywhere leaves everything unset",
			wantTTL:     nil,
			wantEntries: nil,
		},
		{
			name:        "flags apply when neither project nor fleet sets a value",
			flags:       CacheSettings{TTL: ttl(time.Hour), MaxEntries: entries(100)},
			wantTTL:     ttl(time.Hour),
			wantEntries: entries(100),
		},
		{
			name:        "the fleet default outranks the daemon's flags",
			fleet:       CacheSettings{TTL: ttl(2 * time.Hour)},
			flags:       CacheSettings{TTL: ttl(time.Hour)},
			wantTTL:     ttl(2 * time.Hour),
			wantEntries: nil,
		},
		{
			name:        "a project override outranks the fleet default",
			project:     CacheSettings{TTL: ttl(30 * time.Minute)},
			fleet:       CacheSettings{TTL: ttl(2 * time.Hour)},
			wantTTL:     ttl(30 * time.Minute),
			wantEntries: nil,
		},
		{
			// The reason fields resolve independently rather than one level
			// winning wholesale: overriding a TTL must not silently drop the
			// fleet's size cap.
			name:        "fields resolve independently across levels",
			project:     CacheSettings{TTL: ttl(30 * time.Minute)},
			fleet:       CacheSettings{MaxEntries: entries(50)},
			flags:       CacheSettings{TTL: ttl(time.Hour), MaxEntries: entries(999)},
			wantTTL:     ttl(30 * time.Minute),
			wantEntries: entries(50),
		},
		{
			// Zero is a real policy value (never cache), not an absence. If this
			// resolved to the flag's hour, an operator pinning a project to
			// "cache nothing" would silently get an hour of retention.
			name:        "an explicit zero wins over a lower level's non-zero",
			project:     CacheSettings{TTL: ttl(0)},
			flags:       CacheSettings{TTL: ttl(time.Hour)},
			wantTTL:     ttl(0),
			wantEntries: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.project, tc.fleet, tc.flags)
			if !eqDur(got.TTL, tc.wantTTL) {
				t.Errorf("TTL = %v, want %v", showDur(got.TTL), showDur(tc.wantTTL))
			}
			if !eqInt(got.MaxEntries, tc.wantEntries) {
				t.Errorf("MaxEntries = %v, want %v", showInt(got.MaxEntries), showInt(tc.wantEntries))
			}
		})
	}
}

func TestCacheSettings_IsEmpty(t *testing.T) {
	if !(CacheSettings{}).IsEmpty() {
		t.Error("a zero CacheSettings should be empty")
	}
	// An explicit zero is a set value, so the settings are not empty — the same
	// distinction Resolve depends on.
	if (CacheSettings{TTL: ttl(0)}).IsEmpty() {
		t.Error("settings with an explicit zero TTL are not empty")
	}
	// Metadata alone does not make settings non-empty; there is still nothing to
	// apply.
	if !(CacheSettings{UpdatedBy: "nav"}).IsEmpty() {
		t.Error("metadata without any policy value should still be empty")
	}
}

// FleetKey must be unusable as a real project ID, or fleet defaults could be
// shadowed by a project claiming that name.
func TestFleetKeyCannotCollideWithAProject(t *testing.T) {
	if err := project.ValidateID(FleetKey); err == nil {
		t.Errorf("%q must be rejected as a project ID, or it could shadow the fleet defaults", FleetKey)
	}
}

func eqDur(a, b *time.Duration) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func eqInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func showDur(d *time.Duration) string {
	if d == nil {
		return "unset"
	}
	return d.String()
}

func showInt(n *int) string {
	if n == nil {
		return "unset"
	}
	return time.Duration(*n).String()
}
