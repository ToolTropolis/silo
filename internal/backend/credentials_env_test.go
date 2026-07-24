package backend

import "testing"

// TestRuntimeEnv covers the credential precedence for the runtime binaries.
//
// The two-identity split (NAV-110) means silod/dashboard/distil default to the
// least-privilege `silo-runtime` credential, while an operator can still
// override with a single generic pair when they deliberately want one identity.
func TestRuntimeEnv(t *testing.T) {
	const generic, runtime = "SILO_TEST_GENERIC", "SILO_TEST_RUNTIME"

	t.Run("prefers the generic override when set", func(t *testing.T) {
		t.Setenv(generic, "from-generic")
		t.Setenv(runtime, "from-runtime")
		if got := RuntimeEnv(generic, runtime); got != "from-generic" {
			t.Fatalf("want the explicit override to win, got %q", got)
		}
	})

	t.Run("falls back to the runtime-scoped variable", func(t *testing.T) {
		t.Setenv(generic, "")
		t.Setenv(runtime, "from-runtime")
		if got := RuntimeEnv(generic, runtime); got != "from-runtime" {
			t.Fatalf("want the runtime credential, got %q", got)
		}
	})

	t.Run("empty when neither is set", func(t *testing.T) {
		t.Setenv(generic, "")
		t.Setenv(runtime, "")
		if got := RuntimeEnv(generic, runtime); got != "" {
			t.Fatalf("want empty so the caller can report a clear error, got %q", got)
		}
	})
}
