package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/tooltropolis/silo/internal/cache"
)

// fakeLocker records acquire/release calls and can be told who currently holds
// each project, so the daemon's leadership wiring is testable without rqlite.
type fakeLocker struct {
	holder map[string]string // projectID -> owner
}

func newFakeLocker() *fakeLocker { return &fakeLocker{holder: map[string]string{}} }

func (f *fakeLocker) Acquire(_ context.Context, projectID, owner string, _ time.Duration) (bool, error) {
	if h, ok := f.holder[projectID]; ok && h != owner {
		return false, nil // someone else holds it
	}
	f.holder[projectID] = owner
	return true, nil
}

func (f *fakeLocker) Release(_ context.Context, projectID, owner string) error {
	if f.holder[projectID] == owner {
		delete(f.holder, projectID)
	}
	return nil
}

func lockDaemon(t *testing.T, locker *fakeLocker, instanceID string) *Daemon {
	t.Helper()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return New(nil, c, nil, nil).WithLock(locker, instanceID)
}

func TestAcquireLeadership_OneWinnerBetweenTwoDaemons(t *testing.T) {
	ctx := context.Background()
	locker := newFakeLocker()
	d1 := lockDaemon(t, locker, "daemon-1")
	d2 := lockDaemon(t, locker, "daemon-2")

	got1, err := d1.AcquireLeadership(ctx, "proj-11")
	if err != nil || !got1 {
		t.Fatalf("d1 should win the free lock: got=%v err=%v", got1, err)
	}
	// d2 contends for the same project and must lose.
	got2, err := d2.AcquireLeadership(ctx, "proj-11")
	if err != nil {
		t.Fatalf("d2 acquire: %v", err)
	}
	if got2 {
		t.Fatal("two daemons both hold leadership for one project")
	}

	// After d1 releases, d2 can take over.
	if err := d1.ReleaseLeadership(ctx, "proj-11"); err != nil {
		t.Fatalf("d1 release: %v", err)
	}
	got2Now, err := d2.AcquireLeadership(ctx, "proj-11")
	if err != nil || !got2Now {
		t.Fatalf("d2 should acquire after d1 released: got=%v err=%v", got2Now, err)
	}
}

func TestAcquireLeadership_RequiresLocker(t *testing.T) {
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// No WithLock: leadership can't be coordinated and must error rather than
	// silently letting the daemon assume leadership.
	d := New(nil, c, nil, nil)
	if _, err := d.AcquireLeadership(context.Background(), "proj-11"); err == nil {
		t.Fatal("AcquireLeadership without a locker should error")
	}
}
