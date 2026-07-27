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
	// Generation identifies this incarnation of the project. The local cache
	// file is named after the projectID, so a project torn down and re-onboarded
	// under the same ID would otherwise inherit the previous tenant's cached
	// memory. The generation is stamped into the cache file and checked on open.
	//
	// Empty for records created before generations existed; treated as
	// unverifiable rather than as a match.
	Generation string
	// RepoURL and RepoPath record which repository this project serves.
	//
	// Purely informational — nothing in the read, write, or isolation path
	// consults them. They answer "which repo is myservice?" months later,
	// which the project ID alone cannot once it has been normalized away from
	// the repository's own name. Either may be empty.
	RepoURL  string
	RepoPath string
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
	// ClearBucket blanks BucketName after the bucket has actually been deleted,
	// marking that teardown step done. Teardown derives its progress from which
	// refs remain, so the cleared field is what stops deregister from running
	// while the bucket is still live and stranding it.
	ClearBucket(ctx context.Context, projectID string) error
	// SetRepo records which repository a project serves. Separate from Register
	// so onboarding stays a fixed sequence: the repo is metadata attached to a
	// project that already exists, and failing to record it must never fail
	// provisioning.
	SetRepo(ctx context.Context, projectID, repoURL, repoPath string) error
	Deregister(ctx context.Context, projectID string) error // final step of teardown
}

// Status values a ProjectRecord moves through during its lifecycle.
const (
	StatusActive          = "active"
	StatusDecommissioning = "decommissioning"
	StatusDecommissioned  = "decommissioned"
)
