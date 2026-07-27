package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/tooltropolis/silo/internal/cache"
	"github.com/tooltropolis/silo/internal/registry"
)

// SettingsReader is the slice of registry.SettingsStore the daemon needs. It
// only ever reads: the daemon applies policy, the console sets it.
type SettingsReader interface {
	ListSettings(ctx context.Context) (map[string]registry.CacheSettings, error)
}

// PolicySource resolves a project's eviction policy from the registry, falling
// back to the daemon's flags.
//
// Precedence is per-project -> fleet default -> flag. The registry deliberately
// outranks the flag so a console change takes effect without touching every
// host; --cache-config-source=flags pins a host to its flags for debugging.
type PolicySource struct {
	settings SettingsReader
	flags    registry.CacheSettings
	ttl      time.Duration
	logf     func(format string, args ...any)

	mu sync.Mutex
	// known is the last successful read, per project plus the fleet key. It is
	// what a transient registry failure falls back to.
	known     map[string]registry.CacheSettings
	fetchedAt time.Time
	// degraded tracks whether the last refresh failed, so the warning is logged
	// on the transition rather than on every pass.
	degraded bool
	now      func() time.Time
}

// WithEntryLimitFlag sets the daemon's flag-level per-entry cap, the
// lowest-precedence level. Zero leaves it unset so a fleet or project value
// still applies; a positive value is used only when neither of those sets one.
//
// Separate from NewPolicySource's EvictPolicy because the cap is a write-path
// limit, not retention, and cache.EvictPolicy has no field for it — threading
// it through there would put a write concern into the eviction type.
func (p *PolicySource) WithEntryLimitFlag(maxEntryBytes int64) *PolicySource {
	if maxEntryBytes > 0 {
		p.flags.MaxEntryBytes = &maxEntryBytes
	}
	return p
}

// NewPolicySource builds a resolver over the registry.
//
// A nil store means no registry-backed configuration: every project resolves to
// the flags, which is the pre-console behaviour and keeps the quickstart
// working. refreshTTL bounds how often the registry is queried.
func NewPolicySource(s SettingsReader, flags cache.EvictPolicy, refreshTTL time.Duration,
	logf func(string, ...any)) *PolicySource {
	if refreshTTL <= 0 {
		refreshTTL = 30 * time.Second
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &PolicySource{
		settings: s,
		flags:    settingsFromPolicy(flags),
		ttl:      refreshTTL,
		logf:     logf,
		known:    map[string]registry.CacheSettings{},
		now:      time.Now,
	}
}

// Policy returns the effective policy for one project.
func (p *PolicySource) Policy(projectID string) cache.EvictPolicy {
	if p.settings == nil {
		return policyFromSettings(p.flags)
	}
	p.refresh()

	p.mu.Lock()
	project := p.known[projectID]
	fleet := p.known[registry.FleetKey]
	p.mu.Unlock()

	return policyFromSettings(registry.Resolve(project, fleet, p.flags))
}

// MaxEntryBytes returns the per-entry write cap for a project and whether one
// is set at all. It implements EntryLimitSource.
//
// The bool preserves the distinction the nullable column exists for: an
// explicit 0 means "reject every write", and collapsing it into the same value
// as "unset" would turn a lockdown into unlimited.
//
// Shares the resolution path with Policy rather than querying separately: the
// cap inherits the same per-project -> fleet -> flag precedence, the same
// bounded refresh, and the same last-known-good behaviour when the registry is
// briefly unreachable. Falling back to "unlimited" on a blip would drop the
// guard exactly when nobody could see why.
func (p *PolicySource) MaxEntryBytes(projectID string) (int64, bool) {
	if p.settings == nil {
		if p.flags.MaxEntryBytes != nil {
			return *p.flags.MaxEntryBytes, true
		}
		return 0, false
	}
	p.refresh()

	p.mu.Lock()
	project := p.known[projectID]
	fleet := p.known[registry.FleetKey]
	p.mu.Unlock()

	if v := registry.Resolve(project, fleet, p.flags).MaxEntryBytes; v != nil {
		return *v, true
	}
	return 0, false
}

// Refresh forces the next Policy call to re-read the registry, so a console
// write can be reflected without waiting out the TTL.
func (p *PolicySource) Refresh() {
	p.mu.Lock()
	p.fetchedAt = time.Time{}
	p.mu.Unlock()
}

// refresh re-reads the registry when the cached view is older than the TTL.
//
// On failure it keeps the last known-good view rather than falling back to the
// flags. Reverting to flags on a transient error would silently loosen a tight
// cap exactly when the operator cannot see why — the cache would start growing
// again because rqlite blipped.
func (p *PolicySource) refresh() {
	p.mu.Lock()
	fresh := !p.fetchedAt.IsZero() && p.now().Sub(p.fetchedAt) < p.ttl
	p.mu.Unlock()
	if fresh {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	all, err := p.settings.ListSettings(ctx)

	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if !p.degraded {
			p.degraded = true
			p.logf("silod: could not read cache settings (%v); keeping the last known policy", err)
		}
		// Back off a full TTL before trying again rather than hammering a
		// registry that is already struggling.
		p.fetchedAt = p.now()
		return
	}
	if p.degraded {
		p.degraded = false
		p.logf("silod: cache settings readable again")
	}
	p.known = all
	p.fetchedAt = p.now()
}

// policyFromSettings converts resolved settings into the policy the cache
// applies. An unset field becomes zero, which EvictPolicy already reads as
// unlimited.
func policyFromSettings(s registry.CacheSettings) cache.EvictPolicy {
	var pol cache.EvictPolicy
	if s.TTL != nil {
		pol.TTL = *s.TTL
	}
	if s.MaxEntries != nil {
		pol.MaxEntries = *s.MaxEntries
	}
	if s.MaxBytes != nil {
		pol.MaxBytes = *s.MaxBytes
	}
	return pol
}

// settingsFromPolicy lifts the daemon's flags into the settings shape so they
// can take part in Resolve as the lowest-precedence level.
//
// A zero flag is treated as unset, not as an explicit zero: the flags default
// to zero meaning "unlimited", so binding them as set values would make an
// unconfigured daemon override a fleet policy with "no limit".
func settingsFromPolicy(pol cache.EvictPolicy) registry.CacheSettings {
	var s registry.CacheSettings
	if pol.TTL > 0 {
		d := pol.TTL
		s.TTL = &d
	}
	if pol.MaxEntries > 0 {
		n := pol.MaxEntries
		s.MaxEntries = &n
	}
	if pol.MaxBytes > 0 {
		n := pol.MaxBytes
		s.MaxBytes = &n
	}
	return s
}
