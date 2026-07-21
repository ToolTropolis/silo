package backend

import "context"

// SeaweedFS is the default DurableBackend, backed by SeaweedFS's S3-compatible
// gateway: bucket-per-project, native object versioning, ETag conditional
// writes, and a per-project SSE key.
//
// Not yet implemented — see the build sequence in docs/architecture.md
// (Section 8, step 1). The interface it satisfies is stable; this stub exists
// so the module compiles and the CI gate is meaningful from day one.
type SeaweedFS struct {
	// endpoint, region, and per-project credential wiring land here.
}

var _ DurableBackend = (*SeaweedFS)(nil)

func (s *SeaweedFS) Put(ctx context.Context, projectID, path string, content []byte, opts PutOptions) (ObjectVersion, error) {
	return ObjectVersion{}, errNotImplemented
}

func (s *SeaweedFS) Get(ctx context.Context, projectID, path string, versionID string) ([]byte, ObjectVersion, error) {
	return nil, ObjectVersion{}, errNotImplemented
}

func (s *SeaweedFS) ListVersions(ctx context.Context, projectID, path string) ([]ObjectVersion, error) {
	return nil, errNotImplemented
}

func (s *SeaweedFS) Delete(ctx context.Context, projectID, path string) error {
	return errNotImplemented
}

func (s *SeaweedFS) CreateBucket(ctx context.Context, projectID string) error {
	return errNotImplemented
}

func (s *SeaweedFS) DeleteBucket(ctx context.Context, projectID string) error {
	return errNotImplemented
}
