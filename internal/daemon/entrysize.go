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
	// MaxEntryBytes returns the cap in bytes, or 0 for unlimited.
	MaxEntryBytes(projectID string) int64
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
	limit := d.entryLimits.MaxEntryBytes(projectID)
	if limit <= 0 {
		// Zero means unlimited, matching how EvictPolicy reads its own zero
		// values. A project that genuinely wants to reject every write sets a
		// read-only token instead — expressing that as "max 0 bytes" would make
		// an unconfigured daemon indistinguishable from a locked-down one.
		return nil
	}
	if int64(size) > limit {
		return fmt.Errorf("%w: %q is %d bytes, limit is %d — store it as several "+
			"smaller files rather than one large one", ErrEntryTooLarge, path, size, limit)
	}
	return nil
}
