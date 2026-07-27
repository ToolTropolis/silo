package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tooltropolis/silo/internal/cache"
	"github.com/tooltropolis/silo/internal/registry"
)

func ttl(d time.Duration) *time.Duration { return &d }
func entries(n int) *int                 { return &n }

// fakeSettings is a SettingsReader whose responses and failures the test drives.
type fakeSettings struct {
	mu    sync.Mutex
	rows  map[string]registry.CacheSettings
	err   error
	calls int
}

func (f *fakeSettings) ListSettings(context.Context) (map[string]registry.CacheSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]registry.CacheSettings{}
	for k, v := range f.rows {
		out[k] = v
	}
	return out, nil
}

func (f *fakeSettings) set(rows map[string]registry.CacheSettings, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.err = rows, err
}

func (f *fakeSettings) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestPolicySource_Precedence(t *testing.T) {
	flags := cache.EvictPolicy{TTL: time.Hour, MaxEntries: 1000}
	fake := &fakeSettings{rows: map[string]registry.CacheSettings{
		registry.FleetKey: {TTL: ttl(2 * time.Hour)},
		"proj-a":          {MaxEntries: entries(5)},
	}}
	p := NewPolicySource(fake, flags, time.Minute, nil)

	// proj-a: its own MaxEntries, the fleet's TTL, nothing from the flags.
	got := p.Policy("proj-a")
	if got.MaxEntries != 5 {
		t.Errorf("MaxEntries = %d, want 5 from the project override", got.MaxEntries)
	}
	if got.TTL != 2*time.Hour {
		t.Errorf("TTL = %v, want the fleet's 2h", got.TTL)
	}

	// proj-b has no row: the fleet TTL applies, and MaxEntries falls all the way
	// through to the flag.
	got = p.Policy("proj-b")
	if got.TTL != 2*time.Hour {
		t.Errorf("TTL = %v, want the fleet's 2h", got.TTL)
	}
	if got.MaxEntries != 1000 {
		t.Errorf("MaxEntries = %d, want the flag's 1000", got.MaxEntries)
	}
}

// With no registry the daemon must behave exactly as it did before settings
// existed — the quickstart runs this way.
func TestPolicySource_NilStoreUsesFlags(t *testing.T) {
	flags := cache.EvictPolicy{TTL: time.Hour, MaxEntries: 42}
	p := NewPolicySource(nil, flags, time.Minute, nil)

	got := p.Policy("anything")
	if got.TTL != time.Hour || got.MaxEntries != 42 {
		t.Errorf("got %+v, want the flags applied unchanged", got)
	}
}

// The failure mode that matters: a registry blip must not silently loosen a cap
// back to the daemon's (typically unlimited) flags.
func TestPolicySource_KeepsLastKnownGoodOnError(t *testing.T) {
	fake := &fakeSettings{rows: map[string]registry.CacheSettings{
		"proj-a": {MaxEntries: entries(5)},
	}}
	// Flags are unlimited, so a fallback to them would be a visible regression.
	p := NewPolicySource(fake, cache.EvictPolicy{}, time.Minute, nil)

	if got := p.Policy("proj-a"); got.MaxEntries != 5 {
		t.Fatalf("MaxEntries = %d, want 5 before the outage", got.MaxEntries)
	}

	fake.set(nil, errors.New("rqlite unreachable"))
	p.Refresh() // force a re-read, which will now fail

	if got := p.Policy("proj-a"); got.MaxEntries != 5 {
		t.Errorf("MaxEntries = %d during a registry outage, want the last known 5 — "+
			"falling back to unlimited would let the cache grow unbounded", got.MaxEntries)
	}
}

// A change written by the console must be picked up without a restart.
func TestPolicySource_PicksUpChanges(t *testing.T) {
	fake := &fakeSettings{rows: map[string]registry.CacheSettings{
		"proj-a": {MaxEntries: entries(5)},
	}}
	p := NewPolicySource(fake, cache.EvictPolicy{}, time.Minute, nil)

	if got := p.Policy("proj-a"); got.MaxEntries != 5 {
		t.Fatalf("MaxEntries = %d, want 5", got.MaxEntries)
	}

	fake.set(map[string]registry.CacheSettings{"proj-a": {MaxEntries: entries(50)}}, nil)
	p.Refresh()

	if got := p.Policy("proj-a"); got.MaxEntries != 50 {
		t.Errorf("MaxEntries = %d, want the updated 50", got.MaxEntries)
	}
}

// The registry is queried on a TTL, not on every project of every pass — a
// fleet of 50 projects must not mean 50 queries per tick.
func TestPolicySource_CachesWithinTheTTL(t *testing.T) {
	fake := &fakeSettings{rows: map[string]registry.CacheSettings{}}
	p := NewPolicySource(fake, cache.EvictPolicy{}, time.Minute, nil)

	for range 10 {
		p.Policy("proj-a")
		p.Policy("proj-b")
	}
	if n := fake.callCount(); n != 1 {
		t.Errorf("queried the registry %d times, want 1 within the refresh TTL", n)
	}
}

// An explicit zero from the registry must win over a non-zero flag: "cache
// nothing" is a real policy, not an absence.
func TestPolicySource_ExplicitZeroOverridesFlags(t *testing.T) {
	fake := &fakeSettings{rows: map[string]registry.CacheSettings{
		"proj-a": {TTL: ttl(0)},
	}}
	p := NewPolicySource(fake, cache.EvictPolicy{TTL: time.Hour}, time.Minute, nil)

	if got := p.Policy("proj-a"); got.TTL != 0 {
		t.Errorf("TTL = %v, want an explicit 0 to override the flag's hour", got.TTL)
	}
}

// A zero flag must not act as an explicit "unlimited" that shadows fleet policy.
func TestPolicySource_ZeroFlagsDoNotShadowTheFleet(t *testing.T) {
	fake := &fakeSettings{rows: map[string]registry.CacheSettings{
		registry.FleetKey: {MaxEntries: entries(10)},
	}}
	p := NewPolicySource(fake, cache.EvictPolicy{}, time.Minute, nil)

	if got := p.Policy("proj-a"); got.MaxEntries != 10 {
		t.Errorf("MaxEntries = %d, want the fleet's 10 — an unset flag must not "+
			"override it with unlimited", got.MaxEntries)
	}
}

func TestPolicySource_ConcurrentPolicyIsSafe(t *testing.T) {
	fake := &fakeSettings{rows: map[string]registry.CacheSettings{
		"proj-a": {MaxEntries: entries(5)},
	}}
	p := NewPolicySource(fake, cache.EvictPolicy{}, time.Millisecond, nil)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				p.Policy("proj-a")
				p.Refresh()
			}
		}()
	}
	wg.Wait()
}

// The cap resolves through the same precedence as retention: per-project beats
// fleet beats flag. A project override that did not win would let a fleet
// default silently cap a project the operator had exempted.
func TestPolicySource_MaxEntryBytesPrecedence(t *testing.T) {
	i64 := func(n int64) *int64 { return &n }

	for _, tc := range []struct {
		name    string
		project *int64
		fleet   *int64
		flag    int64
		want    int64
	}{
		{"project wins over fleet and flag", i64(100), i64(200), 300, 100},
		{"fleet wins over flag", nil, i64(200), 300, 200},
		{"flag is the fallback", nil, nil, 300, 300},
		{"nothing set is unlimited", nil, nil, 0, 0},
		// An explicit zero is a real value meaning "reject every write" and must
		// not be mistaken for "inherit" — the whole reason the columns are
		// nullable.
		{"an explicit project zero beats a fleet cap", i64(0), i64(200), 300, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := &fakeSettings{}
			settings.set(map[string]registry.CacheSettings{
				"proj-a":          {MaxEntryBytes: tc.project},
				registry.FleetKey: {MaxEntryBytes: tc.fleet},
			}, nil)

			p := NewPolicySource(settings, cache.EvictPolicy{}, time.Minute, nil).
				WithEntryLimitFlag(tc.flag)

			if got := p.MaxEntryBytes("proj-a"); got != tc.want {
				t.Errorf("MaxEntryBytes = %d, want %d", got, tc.want)
			}
		})
	}
}

// A registry blip must not drop the cap. Falling back to unlimited would remove
// the guard exactly when nobody could see why.
func TestPolicySource_MaxEntryBytesKeepsLastKnownOnFailure(t *testing.T) {
	i64 := func(n int64) *int64 { return &n }
	settings := &fakeSettings{}
	settings.set(map[string]registry.CacheSettings{
		"proj-a": {MaxEntryBytes: i64(100)},
	}, nil)

	p := NewPolicySource(settings, cache.EvictPolicy{}, time.Nanosecond, nil)
	if got := p.MaxEntryBytes("proj-a"); got != 100 {
		t.Fatalf("setup: MaxEntryBytes = %d, want 100", got)
	}

	settings.set(nil, errors.New("rqlite unreachable"))
	if got := p.MaxEntryBytes("proj-a"); got != 100 {
		t.Errorf("MaxEntryBytes = %d after a registry failure, want the last known 100 — "+
			"falling back to unlimited would drop the cap during an outage", got)
	}
}
