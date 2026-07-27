package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// seaweedEndpoint is where the integration test looks for a SeaweedFS S3
// gateway. Override with SILO_TEST_S3_ENDPOINT; defaults to the dev compose.
func seaweedEndpoint() string {
	if e := os.Getenv("SILO_TEST_S3_ENDPOINT"); e != "" {
		return e
	}
	return "http://localhost:8333"
}

// newLiveBackend returns an adapter against a reachable SeaweedFS, or skips the
// test when none is up — so `go test ./...` stays green without Docker.
func newLiveBackend(t *testing.T) *SeaweedFS {
	t.Helper()
	endpoint := seaweedEndpoint()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("bad endpoint %q: %v", endpoint, err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 500*time.Millisecond)
	if err != nil {
		t.Skipf("SeaweedFS not reachable at %s (%v) — skipping integration test; "+
			"run `docker compose -f deploy/docker-compose.yaml up -d` to enable", endpoint, err)
	}
	_ = conn.Close()

	ak, sk := testAdminCreds()
	b, err := NewSeaweedFS(Config{Endpoint: endpoint, Region: "us-east-1", AccessKey: ak, SecretKey: sk})
	if err != nil {
		t.Fatalf("NewSeaweedFS: %v", err)
	}
	return b
}

// testAdminCreds returns the silo-admin S3 credentials the dev stack is
// bootstrapped with (deploy/bootstrap-dev.sh). Overridable via env. Once any
// identity exists in SeaweedFS, anonymous access is disabled, so integration
// tests must authenticate.
func testAdminCreds() (accessKey, secretKey string) {
	ak := os.Getenv("SILO_TEST_S3_ACCESS_KEY")
	if ak == "" {
		ak = "SILOADMIN"
	}
	sk := os.Getenv("SILO_TEST_S3_SECRET_KEY")
	if sk == "" {
		sk = "SILOADMINSECRET"
	}
	return ak, sk
}

// uniqueProject gives each test run its own bucket so parallel/repeat runs don't
// collide, and cleans it up afterward.
func uniqueProject(t *testing.T, b *SeaweedFS) string {
	t.Helper()
	proj := fmt.Sprintf("test-%d", time.Now().UnixNano())
	ctx := context.Background()
	if err := b.CreateBucket(ctx, proj); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort teardown; a leaked test bucket is harmless in dev.
		_ = b.Delete(context.Background(), proj, "memory/notes.md")
		_ = b.DeleteBucket(context.Background(), proj)
	})
	return proj
}

func TestSeaweedFS_PutGetDeleteVersions(t *testing.T) {
	b := newLiveBackend(t)
	ctx := context.Background()
	proj := uniqueProject(t, b)
	const path = "memory/notes.md"

	// Get missing -> ErrNotFound.
	if _, _, err := b.Get(ctx, proj, path, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: want ErrNotFound, got %v", err)
	}

	// Put v1.
	v1, err := b.Put(ctx, proj, path, []byte("# v1"), PutOptions{Actor: "agent-1", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if v1.ETag == "" {
		t.Fatal("Put v1: empty ETag")
	}

	// Get returns v1 content + matching ETag.
	got, ver, err := b.Get(ctx, proj, path, "")
	if err != nil {
		t.Fatalf("Get v1: %v", err)
	}
	if string(got) != "# v1" {
		t.Fatalf("Get v1: want %q, got %q", "# v1", got)
	}
	if ver.ETag != v1.ETag {
		t.Fatalf("ETag mismatch: put %q, get %q", v1.ETag, ver.ETag)
	}

	// Put v2 (unconditional overwrite).
	if _, err := b.Put(ctx, proj, path, []byte("# v2"), PutOptions{}); err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	got, _, _ = b.Get(ctx, proj, path, "")
	if string(got) != "# v2" {
		t.Fatalf("Get v2: want %q, got %q", "# v2", got)
	}

	// ListVersions should show at least the two we wrote.
	versions, err := b.ListVersions(ctx, proj, path)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) < 2 {
		t.Fatalf("ListVersions: want >=2, got %d", len(versions))
	}

	// Delete, then Get -> ErrNotFound (delete marker hides the object).
	if err := b.Delete(ctx, proj, path); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := b.Get(ctx, proj, path, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
}

func TestSeaweedFS_PreconditionFailed(t *testing.T) {
	b := newLiveBackend(t)
	ctx := context.Background()
	proj := uniqueProject(t, b)
	const path = "memory/notes.md"

	// Seed an object and capture its ETag.
	v1, err := b.Put(ctx, proj, path, []byte("original"), PutOptions{})
	if err != nil {
		t.Fatalf("Put seed: %v", err)
	}

	// A conditional write with the CURRENT ETag succeeds.
	v2, err := b.Put(ctx, proj, path, []byte("first update"), PutOptions{IfMatchETag: v1.ETag})
	if err != nil {
		t.Fatalf("conditional put with current ETag: want success, got %v", err)
	}

	// A conditional write with the STALE ETag (v1, now superseded by v2) must
	// fail with ErrPreconditionFailed — this is the CAS guard SafeWrite relies on.
	_, err = b.Put(ctx, proj, path, []byte("racing update"), PutOptions{IfMatchETag: v1.ETag})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale conditional put: want ErrPreconditionFailed, got %v", err)
	}

	// The losing write must not have landed — content is still v2's.
	got, _, _ := b.Get(ctx, proj, path, "")
	if string(got) != "first update" {
		t.Fatalf("losing write leaked: want %q, got %q", "first update", got)
	}
	_ = v2
}

// TestIsNotFound_StrictClassification: the daemon deletes a cached entry when
// the backend reports not-found, so a transport failure misclassified as a 404
// would silently drop good cache entries during an outage — exactly when the
// cache matters most.
func TestIsNotFound_StrictClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"typed NoSuchKey", &types.NoSuchKey{}, true},
		{"generic error", errors.New("boom"), false},
		{"connection refused", errors.New("dial tcp: connect: connection refused"), false},
		{"connection reset", errors.New("read: connection reset by peer"), false},
		{"nil", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNotFound(tc.err); got != tc.want {
				t.Errorf("isNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
