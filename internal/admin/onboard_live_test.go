package admin

import (
	"context"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/kms"
	"github.com/tooltropolis/silo/internal/registry"
)

// TestOnboard_EndToEnd provisions a real project against the dev stack
// (rqlite + Vault + SeaweedFS) and confirms every resource lands and is
// consistent, then tears it down. Skips cleanly when the stack isn't up.
func TestOnboard_EndToEnd(t *testing.T) {
	rqAddr := envOr("SILO_TEST_RQLITE_ADDRS", "http://localhost:4001")
	vaultAddr := envOr("SILO_TEST_VAULT_ADDR", "http://localhost:8201")
	vaultToken := envOr("SILO_TEST_VAULT_TOKEN", "dev-only-token")
	s3Endpoint := envOr("SILO_TEST_S3_ENDPOINT", "http://localhost:8333")

	rqNodes := splitCSVForTest(rqAddr)
	// Only the first rqlite node needs to be reachable to seed the client.
	requireReachable(t, append([]string{rqNodes[0]}, vaultAddr, s3Endpoint)...)

	ctx := context.Background()
	reg, err := registry.NewRqlite(ctx, rqNodes)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(reg.Close)
	km, err := kms.NewVault(ctx, kms.Config{Address: vaultAddr, Token: vaultToken})
	if err != nil {
		t.Fatalf("kms: %v", err)
	}
	adminAK, adminSK := testAdminCreds()
	be, err := backend.NewSeaweedFS(backend.Config{
		Endpoint: s3Endpoint, Region: "us-east-1", AccessKey: adminAK, SecretKey: adminSK,
	})
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	o := &Onboarder{Registry: reg, KMS: km, Backend: be, Creds: NoopCredentialIssuer{}}

	proj := "e2e-" + timestamp()
	if err := o.Onboard(ctx, proj); err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	// Ensure teardown even if asserts fail.
	t.Cleanup(func() {
		rec, _ := reg.Get(context.Background(), proj)
		_ = be.DeleteBucket(context.Background(), proj)
		if rec.KeyID != "" {
			_ = km.RevokeKey(context.Background(), rec.KeyID)
		}
		_ = reg.Deregister(context.Background(), proj)
	})

	// Registry record is present with refs populated.
	rec, err := reg.Get(ctx, proj)
	if err != nil {
		t.Fatalf("Get after onboard: %v", err)
	}
	if rec.Status != registry.StatusActive {
		t.Fatalf("status: want active, got %q", rec.Status)
	}
	if rec.KeyID == "" || rec.CredentialID == "" {
		t.Fatalf("refs not persisted: keyID=%q credID=%q", rec.KeyID, rec.CredentialID)
	}

	// The key actually exists and is usable.
	material, err := km.GetKey(ctx, rec.KeyID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if len(material) == 0 {
		t.Fatal("key material empty")
	}

	// The bucket exists — a Put/Get round-trips.
	if _, err := be.Put(ctx, proj, "hello.md", []byte("hi"), backend.PutOptions{}); err != nil {
		t.Fatalf("Put into onboarded bucket: %v", err)
	}
	got, _, err := be.Get(ctx, proj, "hello.md", "")
	if err != nil || string(got) != "hi" {
		t.Fatalf("Get from onboarded bucket: got %q err %v", got, err)
	}
	_ = be.Delete(ctx, proj, "hello.md")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func requireReachable(t *testing.T, addrs ...string) {
	t.Helper()
	for _, a := range addrs {
		u, err := url.Parse(a)
		if err != nil {
			t.Fatalf("bad addr %q: %v", a, err)
		}
		conn, err := net.DialTimeout("tcp", u.Host, 500*time.Millisecond)
		if err != nil {
			t.Skipf("service not reachable at %s (%v) — skipping; run "+
				"`docker compose -f deploy/docker-compose.yaml up -d`", a, err)
		}
		_ = conn.Close()
	}
}

func splitCSVForTest(s string) []string {
	// Reuse the same simple CSV split shape the CLI uses.
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// timestamp builds the unique suffix live tests append to project IDs. It uses
// no separator between the seconds and nanoseconds because the result becomes a
// project ID, and a "." is illegal in an S3 bucket name — these tests generated
// IDs that could never have been valid buckets until ValidateID caught it.
func timestamp() string {
	return time.Now().UTC().Format("20060102150405000000000")
}
