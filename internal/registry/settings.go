package registry

import (
	"context"
	"time"
)

// FleetKey is the reserved project_id under which fleet-wide defaults are
// stored.
//
// Provably collision-free with a real project: project.ValidateID permits only
// lowercase alphanumerics and hyphens, so no project can ever be named
// "_fleet". That is why the defaults share the table rather than needing one of
// their own.
const FleetKey = "_fleet"

// CacheSettings is one level of a project's storage policy.
//
// Named for the cache because retention was all it held originally;
// MaxEntryBytes is a write-path limit rather than retention, and shares the
// struct because it needs exactly the same precedence and refresh machinery
// (see PolicySource). Renaming the type would churn every call site for no
// behavioural gain.
//
// Every field is a pointer so "unset" is distinguishable from "set to zero".
// The distinction is not academic here: zero is a meaningful policy value (a
// TTL of zero means never cache, a MaxEntryBytes of zero rejects every write),
// so a plain int64 could not express "inherit from the next level down".
// Resolve relies on this.
type CacheSettings struct {
	TTL        *time.Duration
	MaxEntries *int
	MaxBytes   *int64
	// MaxEntryBytes caps the size of a single memory entry. Unlike the fields
	// above it bounds what may be written, not what is retained.
	MaxEntryBytes *int64

	UpdatedAt string
	UpdatedBy string
}

// IsEmpty reports whether the settings carry no values at all, which is what a
// project with no row looks like.
func (s CacheSettings) IsEmpty() bool {
	return s.TTL == nil && s.MaxEntries == nil && s.MaxBytes == nil && s.MaxEntryBytes == nil
}

// Resolve layers settings in precedence order, most specific first. Each field
// is taken from the first level that sets it, so a project that overrides only
// the TTL still inherits the fleet's size caps.
//
// Callers pass (project, fleet, flags) in that order. The registry deliberately
// outranks the daemon's flags: an operator changing a policy in the console
// expects it to take effect without touching every host.
func Resolve(levels ...CacheSettings) CacheSettings {
	var out CacheSettings
	for _, l := range levels {
		if out.TTL == nil && l.TTL != nil {
			out.TTL = l.TTL
		}
		if out.MaxEntries == nil && l.MaxEntries != nil {
			out.MaxEntries = l.MaxEntries
		}
		if out.MaxBytes == nil && l.MaxBytes != nil {
			out.MaxBytes = l.MaxBytes
		}
		if out.MaxEntryBytes == nil && l.MaxEntryBytes != nil {
			out.MaxEntryBytes = l.MaxEntryBytes
		}
	}
	return out
}

// SettingsStore persists per-project and fleet-wide cache policy.
//
// Separate from TenantRegistry rather than widening it: that interface is
// documented as the project -> bucket/credential/key mapping, and it is what
// onboarding, teardown, and the daemon's generation check depend on. Retention
// policy is mutable operator configuration with a different lifecycle, and
// every implementation of TenantRegistry would otherwise have to grow methods
// it has no reason to serve.
type SettingsStore interface {
	// GetSettings returns one project's stored settings. A project with no row
	// yields an empty CacheSettings and a nil error — absence is not an error,
	// it means "inherit".
	GetSettings(ctx context.Context, projectID string) (CacheSettings, error)
	// ListSettings returns every stored row, including the fleet defaults,
	// keyed by project ID. Used by the console to render the fleet at once
	// rather than issuing a query per project.
	ListSettings(ctx context.Context) (map[string]CacheSettings, error)
	// PutSettings writes one project's settings, replacing any existing row.
	// A nil field clears that override, restoring inheritance.
	PutSettings(ctx context.Context, projectID string, s CacheSettings) error
	// DeleteSettings removes a project's row entirely, restoring full
	// inheritance. Deleting an absent row is a no-op, not an error.
	DeleteSettings(ctx context.Context, projectID string) error
}
