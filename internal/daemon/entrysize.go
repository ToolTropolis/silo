package daemon

import (
	"errors"
	"fmt"
)

// ErrEntryTooLarge is returned when a write exceeds the project's per-entry
// size cap.
//
// Distinct from a generic failure because it is a policy decision the caller
// can act on: the content is too big, and retrying the same content will never
// succeed. An agent has to be able to tell that apart from a transient backend
// error, which it should retry.
var ErrEntryTooLarge = errors.New("daemon: entry exceeds the size limit")

// EntryLimitSource reports the per-entry write cap for a project.
//
// Narrow on purpose: the daemon applies the limit, the console sets it.
// PolicySource satisfies this, which is how the limit inherits the same
// per-project -> fleet -> flag precedence and last-known-good-on-failure
// behaviour as the retention policy.
type EntryLimitSource interface {
	// MaxEntryBytes returns the cap in bytes and whether one is set at all.
	//
	// The bool is load-bearing and not a convenience. A cap of 0 is a real
	// policy — "reject every write", the control an operator reaches for after
	// a leak — and it is stored as a nullable column precisely so it stays
	// distinguishable from "unset". Returning a bare int64 collapsed the two
	// and made an explicit 0 mean unlimited: the exact opposite of what the
	// operator asked for.
	MaxEntryBytes(projectID string) (limit int64, set bool)
}

// WithEntryLimits configures the per-entry size cap. Optional: a daemon without
// one enforces no limit, which is the behaviour before this existed and keeps
// the quickstart working.
func (d *Daemon) WithEntryLimits(s EntryLimitSource) *Daemon {
	d.entryLimits = s
	return d
}

// checkEntrySize refuses a write whose content exceeds the project's cap.
//
// Reports the actual and permitted sizes. An agent told only "too large" cannot
// tell whether to split the file in two or in fifty, and the operator reading
// the log cannot tell whether the cap is set wrong.
func (d *Daemon) checkEntrySize(projectID, path string, size int) error {
	if d.entryLimits == nil {
		return nil
	}
	limit, set := d.entryLimits.MaxEntryBytes(projectID)
	if !set {
		// No policy at any level: unlimited, which is what an unconfigured
		// daemon has always done.
		return nil
	}
	if limit == 0 {
		// An explicit zero rejects everything. Distinct from unset, and stated
		// separately so the message does not read as a size complaint about a
		// zero-byte limit.
		return fmt.Errorf("%w: writes to %q are disabled by policy (max entry size is 0)",
			ErrEntryTooLarge, path)
	}
	if limit < 0 {
		// Negative is not expressible through the console and has no meaning;
		// treat it as unset rather than rejecting every write on a typo in a
		// hand-edited registry row.
		return nil
	}
	if int64(size) > limit {
		return fmt.Errorf("%w: %q is %d bytes, limit is %d — store it as several "+
			"smaller files rather than one large one", ErrEntryTooLarge, path, size, limit)
	}
	return nil
}
