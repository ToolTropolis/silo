// Command silo-mcp exposes one project's Silo memory to an agent over the
// Model Context Protocol.
//
// This is the piece that makes Silo usable from a repo. Everything else —
// isolation, versioning, caching, consolidation — exists to serve an agent that
// can actually read and write memory, and until now the only way to do that was
// to hand-write curl calls. An agent runtime that speaks MCP can now discover
// four tools (`silo_read`, `silo_write`, `silo_list`, `silo_search`) and use
// them without knowing Silo exists.
//
// One server serves exactly one project, because a token resolves to exactly
// one project on the daemon. That is the isolation boundary, and running one
// process per project keeps it — a server holding project A's token cannot
// address project B's memory even if a tool call asks it to.
//
// It speaks stdio, so the agent runtime launches it as a subprocess and no port
// is opened. Configure it in a repo's .mcp.json (see docs/onboarding-a-repo.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tooltropolis/silo/internal/mcpserver"
	"github.com/tooltropolis/silo/internal/project"
	"github.com/tooltropolis/silo/pkg/client"
)

// version is reported to the MCP client during initialization.
const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		// stdout is the MCP transport, so diagnostics must go to stderr or they
		// corrupt the protocol stream.
		fmt.Fprintln(os.Stderr, "silo-mcp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("silo-mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	daemon := fs.String("daemon", envOr("SILO_DAEMON_ADDR", "http://127.0.0.1:8500"),
		"silod address: an http(s) URL or a Unix socket path (or SILO_DAEMON_ADDR)")
	token := fs.String("token", os.Getenv("SILO_TOKEN"),
		"project-scoped bearer token (or SILO_TOKEN). Prefer the environment variable: "+
			"a token on the command line is visible in the process list")
	projectID := fs.String("project", os.Getenv("SILO_PROJECT"),
		"project this server serves (or SILO_PROJECT). Used for labelling; the token is "+
			"what actually scopes access")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request timeout against the daemon")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *token == "" {
		return fmt.Errorf("a project-scoped token is required: set SILO_TOKEN " +
			"(preferred) or pass --token")
	}
	// Validate early so a typo is reported at startup rather than as a confusing
	// authorization failure on the first tool call.
	if *projectID != "" {
		if err := project.ValidateID(*projectID); err != nil {
			return fmt.Errorf("bad --project: %w", err)
		}
	}

	memory, err := client.New(client.Config{
		Endpoint: *daemon,
		Token:    *token,
		Timeout:  *timeout,
	})
	if err != nil {
		return fmt.Errorf("connect to the daemon: %w", err)
	}

	label := *projectID
	if label == "" {
		label = "silo"
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "silo",
		Title:   "Silo persistent memory (" + label + ")",
		Version: version,
	}, nil)
	mcpserver.New(memory, label).Register(srv)

	// Stop cleanly on SIGINT/SIGTERM so the agent runtime can shut the
	// subprocess down without it being killed mid-response.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "silo-mcp %s: serving project %q via %s\n", version, label, *daemon)

	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		if ctx.Err() != nil {
			return nil // a signal-initiated shutdown is not a failure
		}
		// The agent runtime closing the pipe is how an MCP session ends
		// normally. Exiting non-zero for it would make every clean shutdown
		// look like a crash in the runtime's logs.
		if isDisconnect(err) {
			return nil
		}
		return err
	}
	return nil
}

// isDisconnect reports whether err is the client having gone away rather than
// something going wrong.
func isDisconnect(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, context.Canceled) {
		return true
	}
	// The SDK reports a closed stdin as a wrapped, unexported error, so the
	// message is the only signal available.
	msg := err.Error()
	return strings.Contains(msg, "EOF") || strings.Contains(msg, "closed")
}

// envOr returns the environment variable, or a fallback when it is unset.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
