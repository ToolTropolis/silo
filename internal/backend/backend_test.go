package backend

import (
	"errors"
	"testing"
)

// TestErrorsAreDistinct guards the sentinel errors callers switch on. The full
// adapter suite (put/get/list-versions/delete, precondition-failed on a
// conflicting IfMatchETag) lands with the SeaweedFS implementation — see the
// v1 definition of done in docs/architecture.md.
func TestErrorsAreDistinct(t *testing.T) {
	if errors.Is(ErrPreconditionFailed, ErrNotFound) {
		t.Fatal("ErrPreconditionFailed and ErrNotFound must be distinct sentinels")
	}
}

func TestSeaweedFSSatisfiesInterface(t *testing.T) {
	var _ DurableBackend = (*SeaweedFS)(nil)
}
