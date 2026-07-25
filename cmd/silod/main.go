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
	cacheTTL := fs.Duration("cache-ttl", 0, "discard cached entries older than this (0 = keep forever)")
	cacheMaxEntries := fs.Int("cache-max-entries", 0, "cap cached paths per project (0 = unlimited)")
	cacheMaxBytes := fs.Int64("cache-max-bytes", 0, "cap cached bytes per project (0 = unlimited)")
	evictInterval := fs.Duration("evict-interval", daemon.DefaultEvictInterval, "how often to apply the cache retention policy")
	cacheConfigSource := fs.String("cache-config-source", "registry", "where cache retention policy comes from: \"registry\" (per-project, then fleet default, then these flags) or \"flags\" (pin this host to its flags, ignoring the console)")
	tokenCacheTTL := fs.Duration("token-cache-ttl", daemon.DefaultTokenCacheTTL,
		"how long a verified agent token stays cached. This is the window in which a revoked token still works, so shorter is safer and costs more registry reads")
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

	// --tokens is optional once the registry can issue them: a daemon with a
	// registry serves every token minted for a project without a restart, which
	// is the point of moving tokens out of a startup flag. Requiring both would
	// mean every new project still needed a redeploy.
	var static daemon.StaticTokenVerifier
	if *tokens != "" {
		var err error
		if static, err = parseTokens(*tokens); err != nil {
			return err
		}
	} else if *rqliteAddrs == "" {
		return fmt.Errorf("no way to authorize agents: pass --tokens, or --rqlite so " +
			"tokens minted at onboarding can be verified")
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

	// Authorization: registry-issued tokens when a token store is available,
	// with --tokens still honoured. Static tokens are checked first and never
	// need the registry, so the dev flow keeps working during an outage.
	var verifier daemon.TokenVerifier = static
	if store, ok := reg.(registry.TokenStore); ok {
		verifier = daemon.NewRegistryTokenVerifier(store, static, *tokenCacheTTL)
		fmt.Printf("silod: verifying agent tokens against the registry (cache %s)\n", *tokenCacheTTL)
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

	projects := syncProjects(context.Background(), reg, static)
	worker := daemon.NewSyncWorker(d, projects, *syncInterval, func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Run(ctx)
	}()

	// Cache retention runs on its own cadence: an unsynced write is urgent, a
	// cache entry slightly past its TTL is not, and eviction takes a write
	// transaction that would otherwise contend with a drain.
	flagPolicy := cache.EvictPolicy{
		TTL:        *cacheTTL,
		MaxEntries: *cacheMaxEntries,
		MaxBytes:   *cacheMaxBytes,
	}

	// Policy normally comes from the registry (per-project, then the fleet
	// default, then these flags), so a console change takes effect without
	// touching every host. --cache-config-source=flags pins this host to its
	// own flags, which is the escape hatch for debugging a misconfigured fleet.
	var settings daemon.SettingsReader
	switch *cacheConfigSource {
	case "registry":
		if store, ok := reg.(registry.SettingsStore); ok {
			settings = store
		}
	case "flags":
		fmt.Println("silod: cache policy pinned to this host's flags; console settings are ignored")
	default:
		return fmt.Errorf("bad --cache-config-source %q: want \"registry\" or \"flags\"", *cacheConfigSource)
	}

	policies := daemon.NewPolicySource(settings, flagPolicy, *evictInterval,
		func(format string, args ...any) { fmt.Printf(format+"\n", args...) })

	// The worker starts whenever policy could come from the registry, even with
	// no flags set: a fleet default written from the console has to be applied
	// by a daemon that was started with no caps of its own. Only a host with
	// neither a registry nor any flag has nothing to do.
	if settings != nil || !flagPolicy.Unlimited() {
		evictor := daemon.NewEvictWorker(d,
			func() []string { return projects },
			policies.Policy,
			*evictInterval,
			func(format string, args ...any) { fmt.Printf(format+"\n", args...) })
		wg.Add(1)
		go func() {
			defer wg.Done()
			evictor.Run(ctx)
		}()
		if settings != nil {
			fmt.Printf("silod: cache retention every %s (policy from the registry; flag fallback ttl=%s max-entries=%d max-bytes=%d)\n",
				*evictInterval, *cacheTTL, *cacheMaxEntries, *cacheMaxBytes)
		} else {
			fmt.Printf("silod: cache retention every %s (ttl=%s max-entries=%d max-bytes=%d)\n",
				*evictInterval, *cacheTTL, *cacheMaxEntries, *cacheMaxBytes)
		}
	}

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

	// Counts the flag-configured tokens only: registry-issued ones are resolved
	// on demand, so there is no fixed number to report.
	fmt.Printf("silod: listening on %s (%d static token(s), %d project(s), syncing every %s)\n",
		*listen, len(static), len(projects), *syncInterval)

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
