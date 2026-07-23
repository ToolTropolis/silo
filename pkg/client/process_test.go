package client_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/pkg/client"
)

// TestClient_AgainstSeparateDaemonProcess is the NAV-73 acceptance criterion:
// Read/Write/List/Search work against a running daemon **from a separate
// process**. It builds silod, runs it as a subprocess pointed at the dev
// SeaweedFS, and drives it purely through the public SDK.
//
// Skips when SeaweedFS isn't reachable, so `go test ./...` stays green without
// the dev stack.
func TestClient_AgainstSeparateDaemonProcess(t *testing.T) {
	s3Endpoint := os.Getenv("SILO_TEST_S3_ENDPOINT")
	if s3Endpoint == "" {
		s3Endpoint = "http://localhost:8333"
	}
	if conn, err := net.DialTimeout("tcp", "localhost:8333", 500*time.Millisecond); err != nil {
		t.Skipf("SeaweedFS not reachable (%v) — skipping separate-process test; "+
			"run `docker compose -f deploy/docker-compose.yaml up -d && deploy/bootstrap-dev.sh`", err)
	} else {
		_ = conn.Close()
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	// Build silod into a temp dir.
	bin := filepath.Join(t.TempDir(), "silod")
	build := exec.Command("go", "build", "-o", bin, "./cmd/silod")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build silod: %v\n%s", err, out)
	}

	// The daemon needs a project whose bucket exists; create it via the S3
	// admin creds the dev stack is bootstrapped with.
	project := fmt.Sprintf("sdkproc-%d", time.Now().UnixNano())
	createBucket(t, s3Endpoint, project)

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	const token = "test-token"

	cmd := exec.Command(bin,
		"--listen", addr,
		"--cache-dir", filepath.Join(t.TempDir(), "cache"),
		"--backend-endpoint", s3Endpoint,
		"--tokens", token+"="+project,
		"--s3-access-key", adminAccessKey(),
		"--s3-secret-key", adminSecretKey(),
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start silod: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	waitForDaemon(t, addr)

	// Everything below goes through the public SDK against the separate process.
	c, err := client.New(client.Config{Endpoint: "http://" + addr, Token: token})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	ctx := context.Background()

	want := []byte("# cross-process note\n\nwritten via the SDK")
	if err := c.Write(ctx, "memory/proc.md", want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := c.Read(ctx, "memory/proc.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Read: want %q, got %q", want, got)
	}

	paths, err := c.List(ctx, "memory/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(paths) != 1 || paths[0] != "memory/proc.md" {
		t.Fatalf("List: unexpected %v", paths)
	}

	hits, err := c.Search(ctx, "memory/", "cross-process")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Path != "memory/proc.md" {
		t.Fatalf("Search: unexpected %v", hits)
	}
}

// createBucket provisions the project's bucket so the daemon has somewhere to
// write. Uses the backend adapter with the dev stack's admin credentials.
func createBucket(t *testing.T, endpoint, project string) {
	t.Helper()
	be, err := backend.NewSeaweedFS(backend.Config{
		Endpoint:  endpoint,
		Region:    "us-east-1",
		AccessKey: adminAccessKey(),
		SecretKey: adminSecretKey(),
	})
	if err != nil {
		t.Fatalf("backend for bucket setup: %v", err)
	}
	if err := be.CreateBucket(context.Background(), project); err != nil {
		t.Fatalf("CreateBucket %s: %v", project, err)
	}
	t.Cleanup(func() { _ = be.DeleteBucket(context.Background(), project) })
}

func adminAccessKey() string {
	if v := os.Getenv("SILO_TEST_S3_ACCESS_KEY"); v != "" {
		return v
	}
	return "SILOADMIN"
}

func adminSecretKey() string {
	if v := os.Getenv("SILO_TEST_S3_SECRET_KEY"); v != "" {
		return v
	}
	return "SILOADMINSECRET"
}

// freePort asks the OS for an unused TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitForDaemon polls the health endpoint until the subprocess is serving.
func waitForDaemon(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("daemon did not start listening on %s", addr)
}
