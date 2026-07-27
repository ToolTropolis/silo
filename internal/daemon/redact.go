package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/tooltropolis/silo/internal/registry"
)

// ErrRedactHeadVersion is returned when the requested version is the current
// one. Destroying the head would silently revert the path to older content —
// the reader would see a different memory with no indication anything happened.
// Write a clean version first, then redact the one that leaked.
var ErrRedactHeadVersion = errors.New("daemon: cannot redact the current version")

// ErrVersionNotFound is returned when the named version is not in the path's
// history — already redacted, or never existed.
var ErrVersionNotFound = errors.New("daemon: version not found")

// ErrNoRedactionAudit is returned when the bytes were destroyed but the audit
// row could not be written.
//
// Deliberately loud. The redaction DID happen and cannot be undone, so a caller
// that treats this as an ordinary failure would report "nothing was removed"
// about content that is gone. The operator has to know to record it by hand.
var ErrNoRedactionAudit = errors.New("daemon: redaction succeeded but was not recorded")

// RedactionRecorder persists the audit row. Narrow slice of
// registry.RedactionStore: the daemon writes redactions, the console reads them.
type RedactionRecorder interface {
	RecordRedaction(ctx context.Context, r registry.Redaction) error
}

// WithRedactionAudit configures where redactions are recorded. Redaction is
// refused entirely without it: destroying content with no audit trail is worse
// than not offering the feature.
func (d *Daemon) WithRedactionAudit(rec RedactionRecorder) *Daemon {
	d.redactions = rec
	return d
}

// RedactVersion destroys one version's content and records that it happened.
//
// The order matters and is not reversible: the bytes go first, then the audit
// row. Recording first would leave a claim that content was destroyed when it
// may not have been — a false all-clear about a leaked credential is worse than
// a missing record of a real one.
//
// Refuses the current version: see ErrRedactHeadVersion.
func (d *Daemon) RedactVersion(ctx context.Context, projectID, path, versionID, reason, actor string) error {
	if versionID == "" {
		return fmt.Errorf("%w: no version named", ErrVersionNotFound)
	}
	if d.redactions == nil {
		return errors.New("daemon: no redaction audit configured; refusing to destroy content")
	}

	versions, err := d.backend.ListVersions(ctx, projectID, path)
	if err != nil {
		return fmt.Errorf("daemon: list versions of %q: %w", path, err)
	}
	if len(versions) == 0 {
		return fmt.Errorf("%w: %q has no versions", ErrVersionNotFound, path)
	}

	// ListVersions is newest-first, so index 0 is the head.
	if versions[0].VersionID == versionID {
		return fmt.Errorf("%w: %q is the current content of %q — write a replacement "+
			"first, then redact this version", ErrRedactHeadVersion, versionID, path)
	}
	found := false
	for _, v := range versions {
		if v.VersionID == versionID {
			found = true
			break
		}
	}
	if !found {
		// Not in the history: either already redacted or never existed. Saying so
		// beats a backend error that looks like a fault.
		return fmt.Errorf("%w: %q is not a version of %q", ErrVersionNotFound, versionID, path)
	}

	if err := d.backend.DeleteVersion(ctx, projectID, path, versionID); err != nil {
		return fmt.Errorf("daemon: redact %s@%s: %w", path, versionID, err)
	}

	// No cache invalidation needed, and that is a consequence of the head guard
	// rather than an oversight: the local cache is keyed by path and holds only
	// the current content, so a historical version cannot be in it. If the head
	// ever became redactable, this would have to purge the entry too.

	// The content is gone from here on. An audit failure cannot undo it, so it
	// is reported as its own error rather than folded into a generic one.
	if err := d.redactions.RecordRedaction(ctx, registry.Redaction{
		ProjectID:  projectID,
		Path:       path,
		VersionID:  versionID,
		Reason:     reason,
		RedactedBy: actor,
	}); err != nil {
		return fmt.Errorf("%w: %s@%s was destroyed but the audit row failed (%v) — "+
			"record it manually", ErrNoRedactionAudit, path, versionID, err)
	}
	return nil
}
