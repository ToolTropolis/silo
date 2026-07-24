package backend

import "os"

// RuntimeEnv resolves S3 credentials for the runtime binaries (silod,
// silo-dashboard, silo-distil), preferring an explicitly-set generic variable
// and falling back to the runtime-scoped one.
//
// The two-identity split (NAV-110): only `siloctl` performs bucket lifecycle
// (CreateBucket/DeleteBucket) and needs the `Admin` action. Everything else does
// object CRUD only, so it runs as `silo-runtime`, which SeaweedFS denies bucket
// creation and deletion. A compromised daemon or dashboard therefore cannot
// destroy a project's bucket.
//
// Precedence lets an operator override with a single pair when they genuinely
// want one credential (SILO_S3_*), while the default path picks up the
// least-privilege one (SILO_RUNTIME_*).
func RuntimeEnv(generic, runtime string) string {
	if v := os.Getenv(generic); v != "" {
		return v
	}
	return os.Getenv(runtime)
}
