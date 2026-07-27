// Package mcpserver exposes one project's Silo memory to an agent as MCP tools.
//
// It is deliberately a thin adapter over pkg/client: the daemon's HTTP API is
// the stable contract, and this package translates it into the shape an MCP
// client expects. Keeping the logic here rather than in cmd/silo-mcp means the
// tool behaviour is testable without spawning a process or speaking stdio, and
// a second runtime (a different agent framework, a plugin) can wrap the same
// daemon API without duplicating any of it.
//
// Scope is one project per server instance, matching the daemon's own
// authorization boundary: a token resolves to exactly one project, so a server
// holding that token cannot address another project's memory even if a tool
// call asks it to.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tooltropolis/silo/pkg/client"
)

// Memory is the slice of the Silo SDK the tools need. Narrowed to an interface
// so tests exercise the tool layer without a daemon.
type Memory interface {
	Read(ctx context.Context, path string) ([]byte, error)
	Write(ctx context.Context, path string, content []byte) error
	// WriteAs records who wrote it. Separate from Write so the Client interface
	// keeps its existing shape for every other caller.
	WriteAs(ctx context.Context, path string, content []byte, actor string) error
	List(ctx context.Context, pathPrefix string) ([]string, error)
	Search(ctx context.Context, pathPrefix, query string) ([]client.SearchResult, error)
}

// Server wires Silo memory into an MCP server.
type Server struct {
	memory  Memory
	project string
}

// New returns a Server bound to one project's memory.
func New(memory Memory, projectID string) *Server {
	return &Server{memory: memory, project: projectID}
}

// --- tool inputs and outputs ------------------------------------------------
//
// The SDK derives each tool's JSON schema from these structs, so the field
// comments and jsonschema tags are what the agent actually sees when deciding
// whether and how to call a tool. They are written for that reader.

type ReadInput struct {
	Path string `json:"path" jsonschema:"Memory path to read, e.g. memory/conventions.md"`
}

type ReadOutput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Found   bool   `json:"found"`
}

type WriteInput struct {
	Path    string `json:"path" jsonschema:"Memory path to write, e.g. memory/conventions.md"`
	Content string `json:"content" jsonschema:"Full markdown content to store at this path. This replaces the whole file, so include everything you want kept."`
	// One MCP server serves a whole repo, so it cannot tell which of the repo's
	// agents is calling. The caller names itself instead, which is what lets an
	// operator see which agent produced which memory.
	Actor string `json:"actor,omitempty" jsonschema:"Who is writing: your agent name, e.g. style-reviewer. Recorded so an operator can see which agent produced this memory."`
}

type WriteOutput struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes_written"`
}

type ListInput struct {
	Prefix string `json:"prefix,omitempty" jsonschema:"Path prefix to list under, e.g. memory/. Omit to list everything."`
}

type ListOutput struct {
	Paths []string `json:"paths"`
	Count int      `json:"count"`
}

type SearchInput struct {
	Query  string `json:"query" jsonschema:"Substring to search for across memory content."`
	Prefix string `json:"prefix,omitempty" jsonschema:"Optional path prefix to restrict the search to."`
}

type SearchHit struct {
	Path    string `json:"path"`
	Snippet string `json:"snippet"`
}

type SearchOutput struct {
	Hits  []SearchHit `json:"hits"`
	Count int         `json:"count"`
}

// Register adds every Silo tool to an MCP server.
//
// Tool names are prefixed `silo_` because an agent may have several MCP servers
// connected at once and an unprefixed `read` would collide with anything else
// offering one.
func (s *Server) Register(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "silo_read",
		Description: "Read a memory file from this project's persistent Silo memory. " +
			"Use this at the start of a task to recall what was learned in earlier " +
			"sessions. Returns found=false rather than an error when nothing is stored " +
			"at the path.",
	}, s.handleRead)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "silo_write",
		Description: "Store a memory file in this project's persistent Silo memory, " +
			"so it survives past the end of this session. Writes replace the whole file: " +
			"read it first and include the existing content you want to keep.",
	}, s.handleWrite)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "silo_list",
		Description: "List the memory paths stored for this project. Use this to " +
			"discover what has been remembered before reading individual files.",
	}, s.handleList)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "silo_search",
		Description: "Search this project's stored memory for a substring and return " +
			"matching paths with snippets. Faster than listing and reading everything " +
			"when looking for a specific fact.",
	}, s.handleSearch)
}

// Instructions is what a client is told about this server at initialize, before
// any tool call.
//
// Tool descriptions only get read when a model is already deciding whether to
// call something; this arrives first and frames the whole session. It is also
// the only portable place to say it — a repo's CLAUDE.md is Claude Code
// specific, and hooks exist in some runtimes and not others, but Instructions
// is in the MCP protocol itself, so every compliant client sees it.
//
// Phrased as a working habit rather than a feature tour: an agent that does not
// read at the start has no memory, and one that never writes leaves none behind.
func Instructions(projectID string) string {
	return "This project has persistent memory in Silo that outlives your session.\n\n" +
		"At the start of a task, call silo_list or silo_read on memory/conventions.md " +
		"to recall what earlier sessions learned about " + projectID + ". Do this before " +
		"asking the user something the project may already have recorded.\n\n" +
		"When you learn something durable — a convention, a decision and its reason, a " +
		"gotcha that cost time — call silo_write to store it, and set actor to your " +
		"agent name so an operator can see which agent recorded what. Prefer " +
		"memory/<topic>.md for facts the whole project shares, and " +
		"memory/agents/<your-name>.md for notes only you need.\n\n" +
		"Writes replace the whole file, so read before writing when adding to one. " +
		"Do not store secrets, credentials, or anything you would not commit."
}

func (s *Server) handleRead(ctx context.Context, _ *mcp.CallToolRequest, in ReadInput) (*mcp.CallToolResult, ReadOutput, error) {
	path := strings.TrimSpace(in.Path)
	if path == "" {
		return errorResult("path is required"), ReadOutput{}, nil
	}

	content, err := s.memory.Read(ctx, path)
	if errors.Is(err, client.ErrNotFound) {
		// Absence is a normal answer to "what do you remember?", not a failure.
		// Returning an error would push the agent into error-handling for the
		// most ordinary case there is: nothing stored yet.
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("No memory stored at %q yet.", path),
			}},
		}, ReadOutput{Path: path, Found: false}, nil
	}
	if err != nil {
		return errorResult(fmt.Sprintf("read %q: %v", path, err)), ReadOutput{}, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(content)}},
	}, ReadOutput{Path: path, Content: string(content), Found: true}, nil
}

func (s *Server) handleWrite(ctx context.Context, _ *mcp.CallToolRequest, in WriteInput) (*mcp.CallToolResult, WriteOutput, error) {
	path := strings.TrimSpace(in.Path)
	if path == "" {
		return errorResult("path is required"), WriteOutput{}, nil
	}
	if in.Content == "" {
		// Silently storing an empty file would look like a successful save while
		// destroying whatever was there, since writes replace the whole file.
		return errorResult("content is required; writing empty content would erase the file"),
			WriteOutput{}, nil
	}

	if err := s.memory.WriteAs(ctx, path, []byte(in.Content), in.Actor); err != nil {
		if errors.Is(err, client.ErrReadOnly) {
			// Say plainly that retrying cannot work. An agent told only "write
			// failed" will try again, and again, for the rest of the session.
			return errorResult(fmt.Sprintf(
				"This session has read-only access to project %q, so %q was not stored. "+
					"You can recall memory but not change it. Do not retry — ask the "+
					"operator for a read-write token if this needs to persist.",
				s.project, path)), WriteOutput{}, nil
		}
		return errorResult(fmt.Sprintf("write %q: %v", path, err)), WriteOutput{}, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("Stored %d bytes at %q in project %q.", len(in.Content), path, s.project),
		}},
	}, WriteOutput{Path: path, Bytes: len(in.Content)}, nil
}

func (s *Server) handleList(ctx context.Context, _ *mcp.CallToolRequest, in ListInput) (*mcp.CallToolResult, ListOutput, error) {
	paths, err := s.memory.List(ctx, in.Prefix)
	if err != nil {
		return errorResult(fmt.Sprintf("list %q: %v", in.Prefix, err)), ListOutput{}, nil
	}
	if len(paths) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "No memory stored for this project yet."}},
		}, ListOutput{Paths: []string{}, Count: 0}, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(paths, "\n")}},
	}, ListOutput{Paths: paths, Count: len(paths)}, nil
}

func (s *Server) handleSearch(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return errorResult("query is required"), SearchOutput{}, nil
	}

	results, err := s.memory.Search(ctx, in.Prefix, query)
	if err != nil {
		return errorResult(fmt.Sprintf("search %q: %v", query, err)), SearchOutput{}, nil
	}

	hits := make([]SearchHit, 0, len(results))
	var b strings.Builder
	for _, r := range results {
		hits = append(hits, SearchHit{Path: r.Path, Snippet: r.Snippet})
		fmt.Fprintf(&b, "%s\n  %s\n", r.Path, r.Snippet)
	}
	if len(hits) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("No memory matched %q.", query),
			}},
		}, SearchOutput{Hits: hits, Count: 0}, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: b.String()}},
	}, SearchOutput{Hits: hits, Count: len(hits)}, nil
}

// errorResult reports a tool-level failure to the agent.
//
// IsError rather than a Go error: a returned error is a protocol fault, which
// the client surfaces as the server misbehaving. A failed *call* is ordinary,
// and the agent should see the reason and be able to retry with different
// arguments.
func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
