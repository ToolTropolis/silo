package registry

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func uniqueLockID() string { return fmt.Sprintf("lock-%d", time.Now().UnixNano()) }

// TestLock_ExclusiveAcquire is the NAV-71 acceptance criterion: two owners
// contend for one project's lock and exactly one wins; the other defers.
func TestLock_ExclusiveAcquire(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	proj := uniqueLockID()
	t.Cleanup(func() {
		_ = r.Release(context.Background(), proj, "owner-a")
		_ = r.Release(context.Background(), proj, "owner-b")
	})

	gotA, err := r.Acquire(ctx, proj, "owner-a", 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire A: %v", err)
	}
	if !gotA {
		t.Fatal("owner-a should have acquired the free lock")
	}

	// A different owner must NOT be able to take a live lock.
	gotB, err := r.Acquire(ctx, proj, "owner-b", 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire B: %v", err)
	}
	if gotB {
		t.Fatal("owner-b took a lock already held by owner-a")
	}

	// The holder can renew (re-acquire its own lock).
	gotAgain, err := r.Acquire(ctx, proj, "owner-a", 30*time.Second)
	if err != nil {
		t.Fatalf("renew A: %v", err)
	}
	if !gotAgain {
		t.Fatal("owner-a should be able to renew its own lock")
	}

	// After A releases, B can take it.
	if err := r.Release(ctx, proj, "owner-a"); err != nil {
		t.Fatalf("Release A: %v", err)
	}
	gotBNow, err := r.Acquire(ctx, proj, "owner-b", 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire B after release: %v", err)
	}
	if !gotBNow {
		t.Fatal("owner-b should acquire after owner-a released")
	}
}

// TestLock_ExpiredLeaseCanBeTakenOver confirms a dead owner's lock (expired
// lease) is takeable by another owner.
func TestLock_ExpiredLeaseCanBeTakenOver(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	proj := uniqueLockID()
	t.Cleanup(func() {
		_ = r.Release(context.Background(), proj, "owner-a")
		_ = r.Release(context.Background(), proj, "owner-b")
	})

	// A takes a lock with a lease already in the past.
	if ok, err := r.Acquire(ctx, proj, "owner-a", -1*time.Second); err != nil || !ok {
		t.Fatalf("Acquire A with past lease: ok=%v err=%v", ok, err)
	}
	// B can take over because A's lease is expired.
	gotB, err := r.Acquire(ctx, proj, "owner-b", 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire B over expired lease: %v", err)
	}
	if !gotB {
		t.Fatal("owner-b should take over an expired lock")
	}
}

// TestLock_ConcurrentContention hammers Acquire from many goroutines for one
// project and asserts exactly one wins — the linearizability guarantee.
func TestLock_ConcurrentContention(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	proj := uniqueLockID()
	t.Cleanup(func() {
		for i := 0; i < 10; i++ {
			_ = r.Release(context.Background(), proj, fmt.Sprintf("owner-%d", i))
		}
	})

	const contenders = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	wg.Add(contenders)
	for i := 0; i < contenders; i++ {
		go func(id int) {
			defer wg.Done()
			ok, err := r.Acquire(ctx, proj, fmt.Sprintf("owner-%d", id), 30*time.Second)
			if err != nil {
				return
			}
			if ok {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("exactly one contender must win the lock, got %d", winners)
	}
}
