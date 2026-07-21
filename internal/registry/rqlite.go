package registry

import "context"

// Rqlite is the default TenantRegistry, backed by a 3-node rqlite cluster
// (SQLite semantics + Raft HA). It follows leader redirects automatically via
// the rqlite/gorqlite client.
//
// Not yet implemented — build sequence step 2 (docs/architecture.md).
type Rqlite struct {
	// cluster addresses and the gorqlite connection land here.
}

var _ TenantRegistry = (*Rqlite)(nil)

func (r *Rqlite) Register(ctx context.Context, rec ProjectRecord) error { return errNotImplemented }

func (r *Rqlite) Get(ctx context.Context, projectID string) (ProjectRecord, error) {
	return ProjectRecord{}, errNotImplemented
}

func (r *Rqlite) List(ctx context.Context) ([]ProjectRecord, error) { return nil, errNotImplemented }

func (r *Rqlite) UpdateStatus(ctx context.Context, projectID string, status string) error {
	return errNotImplemented
}

func (r *Rqlite) Deregister(ctx context.Context, projectID string) error { return errNotImplemented }
