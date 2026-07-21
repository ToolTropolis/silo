package dashboard

// registryView is the dashboard home page: a read-only table of every project
// record from the registry (project ID, status, bucket, credential/key
// references — references only, never raw secrets). It never issues a
// credential, revokes a key, or deletes a bucket; those stay exclusively in
// siloctl teardown's confirmed per-layer CLI flow.
//
// Not yet implemented — build sequence step 6 (docs/architecture.md).
func (s *Server) registerRegistryView() {}
