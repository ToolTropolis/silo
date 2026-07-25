// Command silod is the Silo daemon: it hosts the CAS write path, leader lock,
// and local write queue, syncing project memory between the bbolt cache and the
// durable backend, and serves the pkg/client SDK over HTTP.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
	"github.com/tooltropolis/silo/internal/daemon"
	"github.com/tooltropolis/silo/internal/devstack"
	"github.com/tooltropolis/silo/internal/project"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "silod:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("silod", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8500", "listen address (host:port, or a path for a Unix socket)")
	cacheDir := fs.String("cache-dir", "./data/cache", "directory for per-project bbolt cache files")
	backendEndpoint := fs.String("backend-endpoint", "http://localhost:8333", "SeaweedFS S3 endpoint")
	backendRegion := fs.String("backend-region", "us-east-1", "S3 region (SeaweedFS ignores it)")
	accessKey := fs.String("s3-access-key", backend.RuntimeEnv("SILO_S3_ACCESS_KEY", "SILO_RUNTIME_ACCESS_KEY"), "S3 access key (or SILO_S3_ACCESS_KEY / SILO_RUNTIME_ACCESS_KEY)")
	secretKey := fs.String("s3-secret-key", backend.RuntimeEnv("SILO_S3_SECRET_KEY", "SILO_RUNTIME_SECRET_KEY"), "S3 secret key (or SILO_S3_SECRET_KEY / SILO_RUNTIME_SECRET_KEY)")
	tokens := fs.String("tokens", os.Getenv("SILO_TOKENS"), "comma-separated token=projectID pairs the SDK authenticates with (or SILO_TOKENS)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// On the local dev stack, fall back to the runtime identity that
	// bootstrap-dev.sh provisions — object CRUD only, no bucket lifecycle.
	// Gated on a loopback endpoint so a released binary never carries a
	// built-in credential; see internal/devstack.
	if devstack.IsLocal(*backendEndpoint) {
		if *accessKey == "" {
			*accessKey = devstack.RuntimeKey
		}
		if *secretKey == "" {
			*secretKey = devstack.RuntimeSecret
		}
	}

	if *tokens == "" {
		return fmt.Errorf("--tokens is required: at least one token=projectID pair scoping SDK access")
	}
	verifier, err := parseTokens(*tokens)
	if err != nil {
		return err
	}

	be, err := backend.NewSeaweedFS(backend.Config{
		Endpoint:  *backendEndpoint,
		Region:    *backendRegion,
		AccessKey: *accessKey,
		SecretKey: *secretKey,
	})
	if err != nil {
		return fmt.Errorf("backend: %w", err)
	}

	localCache, err := cache.NewBoltCache(*cacheDir)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	defer localCache.Close()

	// Registry and KMS aren't needed for the SDK read/write surface; they're
	// wired in as the daemon takes on onboarding-aware behavior.
	d := daemon.New(be, localCache, nil, nil)
	srv := daemon.NewServer(d, verifier)

	// Bind first, then announce. Printing before the bind makes a port conflict
	// look like a successful start.
	ln, err := srv.Listen(*listen)
	if err != nil {
		return err
	}
	fmt.Printf("silod: listening on %s (%d token(s))\n", *listen, len(verifier))
	return srv.Serve(ln)
}

// parseTokens turns "tok1=projA,tok2=projB" into a verifier. Each token is
// scoped to exactly one project — the daemon's authorization boundary.
func parseTokens(spec string) (daemon.StaticTokenVerifier, error) {
	v := daemon.StaticTokenVerifier{}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		token, projectID, ok := strings.Cut(pair, "=")
		if !ok || token == "" || projectID == "" {
			return nil, fmt.Errorf("bad --tokens entry %q: want token=projectID", pair)
		}
		// Fail at startup rather than on the first write to that project.
		if err := project.ValidateID(projectID); err != nil {
			return nil, fmt.Errorf("bad --tokens entry %q: %w", pair, err)
		}
		v[token] = projectID
	}
	if len(v) == 0 {
		return nil, fmt.Errorf("--tokens contained no valid token=projectID pairs")
	}
	return v, nil
}
