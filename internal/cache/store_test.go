package cache

import "testing"

// The put/get/delete + enqueue/drain suite lands with the bbolt implementation
// (build sequence step 1). This smoke test keeps the package in the CI graph.
func TestPendingWriteZeroValue(t *testing.T) {
	var w PendingWrite
	if w.Path != "" || w.Content != nil {
		t.Fatal("zero-value PendingWrite should be empty")
	}
}
