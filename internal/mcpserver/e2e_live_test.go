package mcpserver_test

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tooltropolis/silo/internal/mcpserver"
	"github.com/tooltropolis/silo/pkg/client"
)

// This is the acceptance test for the whole product: an agent, speaking MCP,
// using Silo as memory. It proves the two claims that make Silo worth running:
//
//  1. Memory outlives the session that wrote it.
//  2. A project cannot reach another project's memory.
//
// It runs against the live dev stack (daemon + backend + registry) and skips
// when that is not up, so `go test ./...` stays green on a machine with no
// docker running.
//
// Configure with SILO_TEST_DAEMON / SILO_TEST_TOKEN_A / SILO_TEST_TOKEN_B.

func daemonAddr() string {
	if v := os.Getenv("SILO_TEST_DAEMON"); v != "" {
		return v
	}
	return "http://127.0.0.1:8500"
}

// liveSession returns a client session backed by a real daemon connection,
// mirroring what silo-mcp does per project. In-process rather than a
// subprocess: the transport is the SDK's, so this exercises the same tool
// layer without depending on a built binary being on disk.
func liveSession(t *testing.T, token, projectID string) *mcp.ClientSession {
	t.Helper()

	addr := daemonAddr()
	host := strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://")
	conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
	if err != nil {
		t.Skipf("daemon not reachable at %s (%v) — skipping; start the dev stack "+
			"and silod to run this", addr, err)
	}
	_ = conn.Close()

	memory, err := client.New(client.Config{Endpoint: addr, Token: token, Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "silo", Version: "test"}, nil)
	mcpserver.New(memory, projectID).Register(srv)
	go func() { _ = srv.Run(ctx, serverTransport) }()

	c := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
	sess, err := c.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func call(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func tokenA(t *testing.T) string {
	v := os.Getenv("SILO_TEST_TOKEN_A")
	if v == "" {
		t.Skip("SILO_TEST_TOKEN_A not set — skipping the live MCP acceptance test")
	}
	return v
}

// The core promise: what an agent learns in one session is available to the
// next one. Each session is a separate server and client, so nothing is shared
// in memory — the only path between them is Silo.
func TestLive_MemorySurvivesTheSession(t *testing.T) {
	token := tokenA(t)
	const path = "memory/e2e-persistence.md"
	content := "The deploy runs on Fridays. " + time.Now().Format(time.RFC3339)

	writer := liveSession(t, token, "project-a")
	if out := call(t, writer, "silo_write", map[string]any{
		"path": path, "content": content,
	}); !strings.Contains(out, "Stored") {
		t.Fatalf("write did not confirm: %s", out)
	}

	// A second, independent session — the agent equivalent of a new day.
	reader := liveSession(t, token, "project-a")
	got := call(t, reader, "silo_read", map[string]any{"path": path})
	if !strings.Contains(got, "deploy runs on Fridays") {
		t.Errorf("a later session could not read what an earlier one wrote.\ngot: %q", got)
	}
}

// Isolation is the product. A second project must not reach the first one's
// memory by any of the three routes an agent has: reading a known path,
// listing, or searching.
func TestLive_ProjectsCannotReadEachOther(t *testing.T) {
	tokA := tokenA(t)
	tokB := os.Getenv("SILO_TEST_TOKEN_B")
	if tokB == "" {
		t.Skip("SILO_TEST_TOKEN_B not set — skipping the cross-project isolation test")
	}

	const secretPath = "memory/e2e-confidential.md"
	const marker = "PROJECT-A-CONFIDENTIAL"

	a := liveSession(t, tokA, "project-a")
	call(t, a, "silo_write", map[string]any{"path": secretPath, "content": marker})

	b := liveSession(t, tokB, "project-b")

	if got := call(t, b, "silo_read", map[string]any{"path": secretPath}); strings.Contains(got, marker) {
		t.Errorf("ISOLATION FAILURE: project B read project A's memory: %q", got)
	}
	if got := call(t, b, "silo_list", map[string]any{"prefix": "memory/"}); strings.Contains(got, "e2e-confidential") {
		t.Errorf("ISOLATION FAILURE: project B listed project A's path: %q", got)
	}
	if got := call(t, b, "silo_search", map[string]any{"query": marker}); strings.Contains(got, "e2e-confidential") {
		t.Errorf("ISOLATION FAILURE: project B found project A's content: %q", got)
	}

	// Isolation must not mean "broken": B's own memory still has to work, or
	// the test above would pass just as well against a server that refuses
	// everything.
	const ownPath = "memory/e2e-own.md"
	const ownMarker = "PROJECT-B-OWN"
	call(t, b, "silo_write", map[string]any{"path": ownPath, "content": ownMarker})

	b2 := liveSession(t, tokB, "project-b")
	if got := call(t, b2, "silo_read", map[string]any{"path": ownPath}); !strings.Contains(got, ownMarker) {
		t.Errorf("project B could not read its own memory: %q", got)
	}
}
