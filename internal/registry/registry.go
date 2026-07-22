// Package registry is the source of truth for the project -> bucket / credential
// / key mapping. Backed by an rqlite cluster (SQLite semantics, Raft-replicated).
package registry

import "context"

// ProjectRecord is one tenant's row in the registry.
type ProjectRecord struct {
	ProjectID    string
	BucketName   string
	CredentialID string // reference into KMS/secrets, not the raw credential
	KeyID        string // KMS key ID for this project's SSE key
	CreatedAt    string
	Status       string // "active" | "decommissioning" | "decommissioned"
}

// TenantRegistry is the source of truth for project -> bucket/credential/key
// mapping. Backed by an rqlite cluster (SQLite semantics, Raft-replicated).
//
// Divergence from spec §3.3: UpdateRefs is added to the interface. The spec's
// onboarding flow (§4 steps 2 & 4) says the keyID and credentialID are "stored
// on the record" after the key and credential are provisioned, but the §3.3
// interface exposed no method to do so. UpdateRefs fills that gap explicitly
// rather than onboarding reaching around the interface.
type TenantRegistry interface {
	Register(ctx context.Context, rec ProjectRecord) error
	Get(ctx context.Context, projectID string) (ProjectRecord, error)
	List(ctx context.Context) ([]ProjectRecord, error)
	UpdateStatus(ctx context.Context, projectID string, status string) error
	// UpdateRefs sets the KMS keyID and credentialID on a project's record,
	// once those resources have been provisioned during onboarding.
	UpdateRefs(ctx context.Context, projectID, keyID, credentialID string) error
	Deregister(ctx context.Context, projectID string) error // final step of teardown
}

// Status values a ProjectRecord moves through during its lifecycle.
const (
	StatusActive          = "active"
	StatusDecommissioning = "decommissioning"
	StatusDecommissioned  = "decommissioned"
)
