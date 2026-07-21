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
type TenantRegistry interface {
	Register(ctx context.Context, rec ProjectRecord) error
	Get(ctx context.Context, projectID string) (ProjectRecord, error)
	List(ctx context.Context) ([]ProjectRecord, error)
	UpdateStatus(ctx context.Context, projectID string, status string) error
	Deregister(ctx context.Context, projectID string) error // final step of teardown
}

// Status values a ProjectRecord moves through during its lifecycle.
const (
	StatusActive          = "active"
	StatusDecommissioning = "decommissioning"
	StatusDecommissioned  = "decommissioned"
)
