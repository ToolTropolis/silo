// Command silo-dashboard serves the v1 read/review web surface: the tenant
// registry, a memory version browser, and Distilator proposal review.
//
// It is read-only except for one action — promoting an approved Distilator
// proposal, which routes through the daemon's CAS write path. Teardown is never
// exposed here; that stays in siloctl's confirmed per-layer CLI flow.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
	"github.com/tooltropolis/silo/internal/daemon"
	"github.com/tooltropolis/silo/internal/devstack"
	"github.com/tooltropolis/silo/internal/distilator"
	"github.com/tooltropolis/silo/internal/registry"
	"github.com/tooltropolis/silo/web/dashboard"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "silo-dashboard:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("silo-dashboard", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8600", "listen address")
	rqliteAddrs := fs.String("rqlite", "http://localhost:4001", "comma-separated rqlite node addresses")
	backendEndpoint := fs.String("backend-endpoint", "http://localhost:8333", "SeaweedFS S3 endpoint")
	backendRegion := fs.String("backend-region", "us-east-1", "S3 region")
	accessKey := fs.String("s3-access-key", backend.RuntimeEnv("SILO_S3_ACCESS_KEY", "SILO_RUNTIME_ACCESS_KEY"), "S3 access key (or SILO_S3_ACCESS_KEY / SILO_RUNTIME_ACCESS_KEY)")
	secretKey := fs.String("s3-secret-key", backend.RuntimeEnv("SILO_S3_SECRET_KEY", "SILO_RUNTIME_SECRET_KEY"), "S3 secret key (or SILO_S3_SECRET_KEY / SILO_RUNTIME_SECRET_KEY)")
	cacheDir := fs.String("cache-dir", "./data/dashboard-cache", "bbolt cache directory (used by the promote path)")
	daemonAddr := fs.String("daemon", os.Getenv("SILO_DAEMON_ADDR"), "silod address, to report unsynced writes (or SILO_DAEMON_ADDR)")
	daemonTokens := fs.String("tokens", os.Getenv("SILO_TOKENS"), "comma-separated token=projectID pairs for querying the daemon (or SILO_TOKENS)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// On the local dev stack, fall back to the runtime identity that
	// bootstrap-dev.sh provisions. Without credentials the dashboard reads
	// anonymously, which SeaweedFS refuses once any identity exists — the whole
	// registry page renders, then every memory view fails with 403, which reads
	// as a dashboard bug rather than a missing credential. Gated on a loopback
	// endpoint so a released binary never carries a built-in credential.
	if devstack.IsLocal(*backendEndpoint) {
		if *accessKey == "" {
			*accessKey = devstack.RuntimeKey
		}
		if *secretKey == "" {
			*secretKey = devstack.RuntimeSecret
		}
	}

	ctx := context.Background()

	reg, err := registry.NewRqlite(ctx, splitCSV(*rqliteAddrs))
	if err != nil {
		return fmt.Errorf("connect registry: %w", err)
	}
	defer reg.Close()

	be, err := backend.NewSeaweedFS(backend.Config{
		Endpoint:  *backendEndpoint,
		Region:    *backendRegion,
		AccessKey: *accessKey,
		SecretKey: *secretKey,
	})
	if err != nil {
		return fmt.Errorf("connect backend: %w", err)
	}

	localCache, err := cache.NewBoltCache(*cacheDir)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer localCache.Close()

	// The daemon supplies SafeWrite, so promotion gets the same CAS/versioning
	// treatment as any other write.
	d := daemon.New(be, localCache, reg, nil)
	reviewer := distilator.NewReviewer(distilator.NewDaemonStore(d), d)

	// Queue depth deliberately does NOT come from the local daemon object: it
	// would read the dashboard's own cache directory, which is not the one silod
	// writes to, and confidently report 0 while writes sit unsynced. Pointing
	// both at one directory is worse — two processes contending for the same
	// bbolt lock. So ask the daemon over HTTP, or report "?" if not configured.
	var queues dashboard.QueueReader
	if *daemonAddr != "" && *daemonTokens != "" {
		queues = newDaemonQueueReader(*daemonAddr, *daemonTokens)
	}

	srv, err := dashboard.NewServer(reg, be, reviewer, queues)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Printf("silo-dashboard: http://%s\n", *listen)
	return httpSrv.ListenAndServe()
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// daemonQueueReader asks silod for a project's unsynced write count over HTTP.
//
// It holds one token per project because /v1/queue is scoped to the caller's own
// project by design — there is no all-projects view behind an agent token, and
// adding one would put fleet-wide state behind an agent credential.
type daemonQueueReader struct {
	addr   string
	tokens map[string]string // projectID -> token
	client *http.Client
}

func newDaemonQueueReader(addr, tokenSpec string) *daemonQueueReader {
	tokens := map[string]string{}
	for _, pair := range strings.Split(tokenSpec, ",") {
		token, projectID, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && token != "" && projectID != "" {
			tokens[projectID] = token
		}
	}
	return &daemonQueueReader{
		addr:   strings.TrimSuffix(addr, "/"),
		tokens: tokens,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// QueueDepth returns an error for a project it has no token for, so the view
// renders "?" rather than a fabricated 0.
func (q *daemonQueueReader) QueueDepth(ctx context.Context, projectID string) (int, error) {
	token, ok := q.tokens[projectID]
	if !ok {
		return 0, fmt.Errorf("no token configured for project %q", projectID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.addr+"/v1/queue", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := q.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("daemon returned %d", resp.StatusCode)
	}
	var body struct {
		Pending int `json:"pending"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	return body.Pending, nil
}
