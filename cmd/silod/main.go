// Command silod is the Silo daemon: it hosts the CAS write path, leader lock,
// and local write queue, syncing project memory between the bbolt cache and the
// durable backend, and serves the pkg/client SDK over HTTP.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
	"github.com/tooltropolis/silo/internal/daemon"
	"github.com/tooltropolis/silo/internal/devstack"
	"github.com/tooltropolis/silo/internal/project"
	"github.com/tooltropolis/silo/internal/registry"
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
	syncInterval := fs.Duration("sync-interval", daemon.DefaultSyncInterval, "how often to replay locally-queued writes to the backend")
	shutdownTimeout := fs.Duration("shutdown-timeout", 10*time.Second, "how long to wait for in-flight requests and a final queue drain on shutdown")
	rqliteAddrs := fs.String("rqlite", os.Getenv("SILO_RQLITE_ADDRS"), "comma-separated rqlite node addresses (or SILO_RQLITE_ADDRS); optional, but required to verify cache ownership")
	adminListen := fs.String("admin-listen", "", "operator socket for cache stats and purges (a path; empty disables it). Not token-authenticated — the socket's permissions are the boundary, so do not bind it to TCP")
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

	// The registry is optional so the quickstart and single-node dev flow keep
	// working, but without it the daemon cannot check which generation of a
	// project owns a cache file. Say so plainly rather than degrading silently.
	var reg registry.TenantRegistry
	if *rqliteAddrs != "" {
		r, err := registry.NewRqlite(context.Background(), splitCSV(*rqliteAddrs))
		if err != nil {
			return fmt.Errorf("connect registry: %w", err)
		}
		defer r.Close()
		reg = r
	} else {
		fmt.Println("silod: WARNING no --rqlite configured; cache ownership cannot be verified")
	}

	// KMS isn't needed for the SDK read/write surface; it's wired in as the
	// daemon takes on onboarding-aware behavior.
	d := daemon.New(be, localCache, reg, nil)
	srv := daemon.NewServer(d, verifier)

	// Bind first, then announce. Printing before the bind makes a port conflict
	// look like a successful start.
	ln, err := srv.Listen(*listen)
	if err != nil {
		return err
	}

	// Stop on SIGINT/SIGTERM rather than being killed outright. Without this the
	// process dies mid-drain, `defer localCache.Close()` never runs, and buffered
	// writes are left behind with no clean close of the bbolt files.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	projects := syncProjects(context.Background(), reg, verifier)
	worker := daemon.NewSyncWorker(d, projects, *syncInterval, func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Run(ctx)
	}()

	httpSrv := &http.Server{Handler: srv.Handler()}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	// Operator surface, off the agent-facing listener. Destructive and
	// fleet-wide operations must not be reachable with an agent's token.
	var adminSrv *http.Server
	if *adminListen != "" {
		adminLn, err := daemon.Listen(*adminListen)
		if err != nil {
			return fmt.Errorf("admin listener: %w", err)
		}
		admin := daemon.NewAdminServer(d, func() []string { return projects })
		adminSrv = &http.Server{Handler: admin.Handler()}
		go func() { _ = adminSrv.Serve(adminLn) }()
		fmt.Printf("silod: admin socket at %s\n", *adminListen)
	}

	fmt.Printf("silod: listening on %s (%d token(s), %d project(s), syncing every %s)\n",
		*listen, len(verifier), len(projects), *syncInterval)

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	fmt.Println("silod: shutting down")

	// Stop accepting requests BEFORE draining. Draining first would race new
	// writes into the queue for as long as the drain takes.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		fmt.Printf("silod: http shutdown: %v\n", err)
	}
	if adminSrv != nil {
		_ = adminSrv.Shutdown(shutdownCtx)
	}

	wg.Wait() // the ticker goroutine observes the cancelled ctx

	// One last drain so a clean stop doesn't strand writes on local disk. Bounded
	// so a dead backend can't block shutdown indefinitely.
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancelFlush()
	worker.SyncOnce(flushCtx)

	// Report anything still buffered — the operator needs to know this host holds
	// data that never reached the backend.
	for _, projectID := range projects {
		depth, err := d.QueueDepth(flushCtx, projectID)
		if err != nil {
			fmt.Printf("silod: %s: could not read queue depth: %v\n", projectID, err)
			continue
		}
		if depth > 0 {
			fmt.Printf("silod: WARNING %s has %d unsynced write(s) still in %s\n",
				projectID, depth, *cacheDir)
		}
	}
	return nil
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

// splitCSV splits a comma-separated flag value, dropping blanks.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// syncProjects decides which projects the sync worker drains.
//
// Tokens alone miss a project whose token was rotated away mid-outage — its
// queue would then never drain. The registry alone would miss everything if
// rqlite is briefly unreachable at startup. So take the union: the registry is
// authoritative about what exists, and the token set guarantees that a project
// actively being written to is never dropped because of a registry blip.
func syncProjects(ctx context.Context, reg registry.TenantRegistry, verifier daemon.StaticTokenVerifier) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(id string) {
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}

	for _, id := range verifier.Projects() {
		add(id)
	}
	if reg != nil {
		recs, err := reg.List(ctx)
		if err != nil {
			fmt.Printf("silod: could not list projects from the registry (%v); syncing the token set only\n", err)
		} else {
			for _, rec := range recs {
				// A decommissioned project has no bucket to drain into.
				if rec.Status == registry.StatusDecommissioned {
					continue
				}
				add(rec.ProjectID)
			}
		}
	}
	sort.Strings(out)
	return out
}
