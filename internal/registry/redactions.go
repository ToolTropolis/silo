package registry

import "context"

// Redaction records that one object version's content was destroyed.
//
// The version it names no longer exists in the object store — that is the point
// of a redaction, and why this record lives in the registry instead. An audit
// kept beside the bytes would be destroyed with them.
type Redaction struct {
	ProjectID  string
	Path       string
	VersionID  string
	Reason     string
	RedactedAt string
	RedactedBy string
}

// RedactionStore records and reports destroyed versions.
//
// Separate from TenantRegistry for the same reason SettingsStore and TokenStore
// are: that interface is the project -> bucket/credential/key mapping that
// onboarding and teardown depend on, and an audit log has an unrelated
// lifecycle.
//
// There is deliberately no delete: a redaction that could itself be redacted is
// not an audit trail.
type RedactionStore interface {
	// RecordRedaction saves the audit row. Called AFTER the bytes are destroyed,
	// so a failure here leaves a redaction that happened but was not recorded —
	// the caller must surface that loudly rather than swallowing it.
	RecordRedaction(ctx context.Context, r Redaction) error
	// ListRedactions returns a project's redactions, newest first. A path filter
	// narrows it to one memory's history; an empty path returns all of them.
	ListRedactions(ctx context.Context, projectID, path string) ([]Redaction, error)
}
