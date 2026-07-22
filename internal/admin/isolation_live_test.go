package admin

import (
	"bytes"
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/tooltropolis/silo/internal/backend"
)

// dockerWeedRunner runs `weed shell` inside the dev SeaweedFS container. The
// container name is overridable via SILO_TEST_SEAWEED_CONTAINER. This is the
// live-test transport; production configures a real weed binary + filer address
// through SeaweedConfig instead.
func dockerWeedRunner(t *testing.T) shellRunner {
	t.Helper()
	container := os.Getenv("SILO_TEST_SEAWEED_CONTAINER")
	if container == "" {
		container = "deploy-seaweedfs-1"
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH — skipping live isolation test")
	}
	return func(ctx context.Context, command string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", container, "weed", "shell")
		cmd.Stdin = strings.NewReader(command + "\n")
		return cmd.CombinedOutput()
	}
}

func s3ClientFor(endpoint, ak, sk string) *s3.Client {
	return s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(ak, sk, ""),
	})
}

func requireS3(t *testing.T, endpoint string) {
	t.Helper()
	u, _ := url.Parse(endpoint)
	conn, err := net.DialTimeout("tcp", u.Host, 500*time.Millisecond)
	if err != nil {
		t.Skipf("SeaweedFS not reachable at %s (%v) — run `docker compose -f deploy/docker-compose.yaml up -d`", endpoint, err)
	}
	_ = conn.Close()
}

func isDenied(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// SeaweedFS returns 403 AccessDenied for a scope violation.
	return strings.Contains(msg, "403") || strings.Contains(strings.ToLower(msg), "access denied")
}

// TestCrossProjectIsolation is the NAV-78 acceptance criterion, and the core
// product guarantee proven rather than reviewed: a credential issued for
// project A is denied (403) on project B's bucket, for both read and write.
//
// It onboards two real projects (each getting a bucket-scoped SeaweedFS
// identity via the real SeaweedCredentialIssuer), then drives raw signed S3
// requests with A's credential against B's bucket and asserts they're refused.
func TestCrossProjectIsolation(t *testing.T) {
	s3Endpoint := envOr("SILO_TEST_S3_ENDPOINT", "http://localhost:8333")
	requireS3(t, s3Endpoint)

	ctx := context.Background()
	run := dockerWeedRunner(t)
	store := newMemSecretStore()
	issuer := &SeaweedCredentialIssuer{run: run, secret: store}

	adminAK, adminSK := testAdminCreds()
	be, err := backend.NewSeaweedFS(backend.Config{
		Endpoint: s3Endpoint, Region: "us-east-1", AccessKey: adminAK, SecretKey: adminSK,
	})
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	// Two projects with distinct buckets + scoped identities.
	pa := "iso-a-" + timestamp()
	pb := "iso-b-" + timestamp()
	bucketA, bucketB := "silo-"+pa, "silo-"+pb

	for _, p := range []string{pa, pb} {
		if err := be.CreateBucket(ctx, p); err != nil {
			t.Fatalf("CreateBucket %s: %v", p, err)
		}
	}
	credA, err := issuer.Issue(ctx, pa, bucketA)
	if err != nil {
		t.Fatalf("Issue A: %v", err)
	}
	credB, err := issuer.Issue(ctx, pb, bucketB)
	if err != nil {
		t.Fatalf("Issue B: %v", err)
	}
	t.Cleanup(func() {
		_ = issuer.Revoke(context.Background(), credA)
		_ = issuer.Revoke(context.Background(), credB)
		_ = be.DeleteBucket(context.Background(), pa)
		_ = be.DeleteBucket(context.Background(), pb)
	})

	// Pull A's access/secret back out of the store for signing.
	akA, skA := splitCred(store.m[credA])

	clientA := s3ClientFor(s3Endpoint, akA, skA)

	// A can write to its OWN bucket.
	if _, err := clientA.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucketA), Key: aws.String("mine.md"), Body: bytes.NewReader([]byte("A")),
	}); err != nil {
		t.Fatalf("A -> own bucket should be allowed, got: %v", err)
	}

	// A must be DENIED reading B's bucket.
	if _, err := clientA.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucketB), Key: aws.String("mine.md"),
	}); !isDenied(err) {
		t.Fatalf("A -> B READ should be denied, got: %v", err)
	}

	// A must be DENIED writing B's bucket.
	if _, err := clientA.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucketB), Key: aws.String("evil.md"), Body: bytes.NewReader([]byte("intrusion")),
	}); !isDenied(err) {
		t.Fatalf("A -> B WRITE should be denied, got: %v", err)
	}

	// And anonymous access must be denied now that identities exist.
	anon := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(s3Endpoint),
		UsePathStyle: true, Credentials: aws.AnonymousCredentials{},
	})
	if _, err := anon.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucketA), Key: aws.String("mine.md"),
	}); !isDenied(err) {
		t.Fatalf("anonymous read should be denied, got: %v", err)
	}
}

// testAdminCreds returns the silo-admin S3 credentials the dev stack is
// bootstrapped with (deploy/bootstrap-dev.sh). Overridable via env. Once any
// identity exists in SeaweedFS, anonymous access is disabled, so integration
// tests must authenticate the backend adapter.
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

// splitCred splits the "access:secret" value the issuer stores.
func splitCred(v string) (access, secret string) {
	if i := strings.IndexByte(v, ':'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}
