package mcpserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tooltropolis/silo/pkg/client"
)

// fakeMemory is an in-memory Silo, so the tool layer is exercised without a
// daemon, a network, or a bbolt file.
type fakeMemory struct {
	mu    sync.Mutex
	store map[string][]byte

	readErr, writeErr, listErr, searchErr error
	// writes records every write in order, so a test can assert what was stored
	// rather than only what reads back.
	writes []string
}

func newFakeMemory() *fakeMemory {
	return &fakeMemory{store: map[string][]byte{}}
}

func (f *fakeMemory) Read(_ context.Context, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	v, ok := f.store[path]
	if !ok {
		return nil, client.ErrNotFound
	}
	return v, nil
}

func (f *fakeMemory) Write(_ context.Context, path string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.store[path] = content
	f.writes = append(f.writes, path)
	return nil
}

func (f *fakeMemory) List(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []string
	for p := range f.store {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeMemory) Search(_ context.Context, prefix, query string) ([]client.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	var out []client.SearchResult
	for p, v := range f.store {
		if strings.HasPrefix(p, prefix) && strings.Contains(string(v), query) {
			out = append(out, client.SearchResult{Path: p, Snippet: string(v)})
		}
	}
	return out, nil
}

// connect wires a Server to a client over the SDK's in-memory transport, which
// exercises the real protocol path — schema generation, argument unmarshalling,
// result encoding — without spawning a process.
func connect(t *testing.T, mem Memory) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "silo", Version: "test"}, nil)
	New(mem, "proj-11").Register(srv)

	go func() { _ = srv.Run(ctx, serverTransport) }()

	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := c.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// callText runs a tool and returns its text content plus whether it errored.
func callText(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: name, Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

// The four tools must be discoverable with descriptions — that is how an agent
// decides whether to call them at all.
func TestTools_AreDiscoverable(t *testing.T) {
	sess := connect(t, newFakeMemory())

	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	want := map[string]bool{
		"silo_read": false, "silo_write": false, "silo_list": false, "silo_search": false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		want[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %q has no description; an agent cannot tell when to use it", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q was not registered", name)
		}
	}
}

// Tools are prefixed because an agent may have several MCP servers connected,
// and an unprefixed `read` would collide with anything else offering one.
func TestTools_AreNamespaced(t *testing.T) {
	sess := connect(t, newFakeMemory())
	res, _ := sess.ListTools(context.Background(), nil)
	for _, tool := range res.Tools {
		if !strings.HasPrefix(tool.Name, "silo_") {
			t.Errorf("tool %q is not namespaced; it could collide with another server", tool.Name)
		}
	}
}

func TestWriteThenRead(t *testing.T) {
	mem := newFakeMemory()
	sess := connect(t, mem)

	out, isErr := callText(t, sess, "silo_write", map[string]any{
		"path": "memory/notes.md", "content": "remember this",
	})
	if isErr {
		t.Fatalf("write reported an error: %s", out)
	}
	if !strings.Contains(out, "memory/notes.md") {
		t.Errorf("write should confirm the path, got %q", out)
	}

	out, isErr = callText(t, sess, "silo_read", map[string]any{"path": "memory/notes.md"})
	if isErr {
		t.Fatalf("read reported an error: %s", out)
	}
	if out != "remember this" {
		t.Errorf("read returned %q, want the stored content", out)
	}
}

// Absence is the ordinary answer to "what do you remember?" on a fresh project.
// Reporting it as an error would push the agent into error handling for the most
// common case there is.
func TestRead_MissingPathIsNotAnError(t *testing.T) {
	sess := connect(t, newFakeMemory())

	out, isErr := callText(t, sess, "silo_read", map[string]any{"path": "memory/nothing.md"})
	if isErr {
		t.Error("a missing memory must not be reported as a tool error")
	}
	if !strings.Contains(out, "No memory stored") {
		t.Errorf("got %q, want a plain statement that nothing is stored", out)
	}
}

// Writes replace the whole file, so accepting empty content would look like a
// successful save while erasing what was there.
func TestWrite_RejectsEmptyContent(t *testing.T) {
	mem := newFakeMemory()
	sess := connect(t, mem)

	out, isErr := callText(t, sess, "silo_write", map[string]any{
		"path": "memory/notes.md", "content": "",
	})
	if !isErr {
		t.Error("empty content must be rejected")
	}
	if !strings.Contains(out, "erase") {
		t.Errorf("the refusal should explain why, got %q", out)
	}
	if len(mem.writes) != 0 {
		t.Errorf("nothing should have been written, got %v", mem.writes)
	}
}

func TestWrite_RejectsEmptyPath(t *testing.T) {
	mem := newFakeMemory()
	sess := connect(t, mem)

	_, isErr := callText(t, sess, "silo_write", map[string]any{
		"path": "   ", "content": "x",
	})
	if !isErr {
		t.Error("a blank path must be rejected")
	}
	if len(mem.writes) != 0 {
		t.Errorf("nothing should have been written, got %v", mem.writes)
	}
}

func TestList(t *testing.T) {
	mem := newFakeMemory()
	sess := connect(t, mem)

	for _, p := range []string{"memory/a.md", "memory/b.md", "other/c.md"} {
		callText(t, sess, "silo_write", map[string]any{"path": p, "content": "x"})
	}

	out, _ := callText(t, sess, "silo_list", map[string]any{"prefix": "memory/"})
	for _, want := range []string{"memory/a.md", "memory/b.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("list should include %q, got %q", want, out)
		}
	}
	if strings.Contains(out, "other/c.md") {
		t.Errorf("the prefix should have excluded other/c.md, got %q", out)
	}
}

// An empty project should say so plainly rather than returning a bare empty
// string the agent has to interpret.
func TestList_EmptyProjectSaysSo(t *testing.T) {
	sess := connect(t, newFakeMemory())

	out, isErr := callText(t, sess, "silo_list", map[string]any{})
	if isErr {
		t.Error("an empty project is not an error")
	}
	if !strings.Contains(out, "No memory stored") {
		t.Errorf("got %q, want a plain statement", out)
	}
}

func TestSearch(t *testing.T) {
	mem := newFakeMemory()
	sess := connect(t, mem)
	callText(t, sess, "silo_write", map[string]any{
		"path": "memory/prefs.md", "content": "the user prefers tabs",
	})

	out, isErr := callText(t, sess, "silo_search", map[string]any{"query": "tabs"})
	if isErr {
		t.Fatalf("search errored: %s", out)
	}
	if !strings.Contains(out, "memory/prefs.md") {
		t.Errorf("search should return the matching path, got %q", out)
	}
}

func TestSearch_NoMatchIsNotAnError(t *testing.T) {
	sess := connect(t, newFakeMemory())

	out, isErr := callText(t, sess, "silo_search", map[string]any{"query": "nothing"})
	if isErr {
		t.Error("no matches is not an error")
	}
	if !strings.Contains(out, "No memory matched") {
		t.Errorf("got %q", out)
	}
}

func TestSearch_RejectsEmptyQuery(t *testing.T) {
	sess := connect(t, newFakeMemory())

	_, isErr := callText(t, sess, "silo_search", map[string]any{"query": "  "})
	if !isErr {
		t.Error("a blank query must be rejected")
	}
}

// A daemon failure must reach the agent as a failed call it can retry or report,
// not as a protocol error that looks like the server is broken.
func TestDaemonFailure_IsAToolErrorNotAProtocolError(t *testing.T) {
	mem := newFakeMemory()
	mem.readErr = errors.New("connection refused")
	sess := connect(t, mem)

	out, isErr := callText(t, sess, "silo_read", map[string]any{"path": "memory/x.md"})
	if !isErr {
		t.Error("a daemon failure should be reported as a tool error")
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("the underlying cause should be visible, got %q", out)
	}
}

func TestWriteFailure_IsReported(t *testing.T) {
	mem := newFakeMemory()
	mem.writeErr = errors.New("backend unreachable")
	sess := connect(t, mem)

	out, isErr := callText(t, sess, "silo_write", map[string]any{
		"path": "memory/x.md", "content": "data",
	})
	if !isErr {
		t.Error("a failed write must be reported, not silently swallowed")
	}
	if !strings.Contains(out, "backend unreachable") {
		t.Errorf("got %q", out)
	}
}

// The structured output is what a caller reads programmatically, so it must
// agree with the human-readable text rather than drifting from it.
func TestStructuredOutput_MatchesTheText(t *testing.T) {
	mem := newFakeMemory()
	sess := connect(t, mem)
	callText(t, sess, "silo_write", map[string]any{
		"path": "memory/x.md", "content": "hello",
	})

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "silo_read", Arguments: map[string]any{"path": "memory/x.md"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.StructuredContent == nil {
		t.Fatal("read should return structured output")
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T, want an object", res.StructuredContent)
	}
	if m["found"] != true {
		t.Errorf("found = %v, want true", m["found"])
	}
	if m["content"] != "hello" {
		t.Errorf("content = %v, want hello", m["content"])
	}
}
