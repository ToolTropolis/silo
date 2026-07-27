// Command silo-distil runs a Distilator consolidation cycle for a project,
// invoked by silod or on a schedule (e.g. `silo-distil run --project=X --since=24h`).
//
// A run never modifies the live memory store: proposals are written to
// _distilations/<run-id>/ and stay there until a human promotes them with
// `silo-distil promote`.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
	"github.com/tooltropolis/silo/internal/daemon"
	"github.com/tooltropolis/silo/internal/distilator"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCycle(os.Args[2:])
	case "promote":
		err = promote(os.Args[2:])
	case "show":
		err = show(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "silo-distil: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "silo-distil:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `silo-distil — Distilator consolidation cycle

Usage:
  silo-distil run     --project=<id> [--since=24h] [--run-id=<id>] [--instructions=<text>]
  silo-distil show    --project=<id> --run-id=<id>
  silo-distil promote --project=<id> --run-id=<id> --paths=<a.md,b.md>

A run writes proposals to _distilations/<run-id>/ and never touches the live
memory store. Promotion applies approved proposals through the daemon's CAS
write path, tagged promoted_from:<run-id>.

Authentication: the Claude provider uses the standard credential chain — run
"ant auth login" once for subscription-based OAuth (no API key needed).`)
}

// storeFlags are the connection flags every subcommand needs.
type storeFlags struct {
	project   string
	cacheDir  string
	endpoint  string
	region    string
	accessKey string
	secretKey string
}

func addStoreFlags(fs *flag.FlagSet) *storeFlags {
	f := &storeFlags{}
	fs.StringVar(&f.project, "project", "", "project ID (required)")
	// Deliberately NOT silod's ./data/cache. bbolt takes an exclusive lock per
	// file, so pointing both here means whichever process touches a project
	// second blocks for five seconds and then fails with a timeout that looks
	// nothing like its cause. The dashboard and siloctl already avoid sharing
	// silod's directory for the same reason.
	fs.StringVar(&f.cacheDir, "cache-dir", "./data/distil-cache", "bbolt cache directory (must not be silod's --cache-dir; bbolt locks each file exclusively)")
	fs.StringVar(&f.endpoint, "backend-endpoint", "http://localhost:8333", "SeaweedFS S3 endpoint")
	fs.StringVar(&f.region, "backend-region", "us-east-1", "S3 region")
	fs.StringVar(&f.accessKey, "s3-access-key", backend.RuntimeEnv("SILO_S3_ACCESS_KEY", "SILO_RUNTIME_ACCESS_KEY"), "S3 access key (or SILO_S3_ACCESS_KEY / SILO_RUNTIME_ACCESS_KEY)")
	fs.StringVar(&f.secretKey, "s3-secret-key", backend.RuntimeEnv("SILO_S3_SECRET_KEY", "SILO_RUNTIME_SECRET_KEY"), "S3 secret key (or SILO_S3_SECRET_KEY / SILO_RUNTIME_SECRET_KEY)")
	return f
}

// open wires a daemon and the Distilator's store adapter.
func (f *storeFlags) open() (*daemon.Daemon, *distilator.DaemonStore, func(), error) {
	if f.project == "" {
		return nil, nil, nil, fmt.Errorf("--project is required")
	}
	be, err := backend.NewSeaweedFS(backend.Config{
		Endpoint:  f.endpoint,
		Region:    f.region,
		AccessKey: f.accessKey,
		SecretKey: f.secretKey,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("backend: %w", err)
	}
	localCache, err := cache.NewBoltCache(f.cacheDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cache: %w", err)
	}
	d := daemon.New(be, localCache, nil, nil)
	return d, distilator.NewDaemonStore(d), func() { _ = localCache.Close() }, nil
}

func runCycle(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	sf := addStoreFlags(fs)
	since := fs.Duration("since", 24*time.Hour, "how far back to pull transcripts")
	runID := fs.String("run-id", "", "run ID (default: distil-<unix-nano>)")
	instructions := fs.String("instructions", "", "extra guidance for this consolidation run")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, store, closeFn, err := sf.open()
	if err != nil {
		return err
	}
	defer closeFn()

	id := *runID
	if id == "" {
		id = fmt.Sprintf("distil-%d", time.Now().UnixNano())
	}

	orch := distilator.NewOrchestrator(
		distilator.NewClaudeProvider(),
		store,
		distilator.NewStoreTranscripts(store),
	)

	run, err := orch.Run(context.Background(), sf.project, id, int(since.Hours()), *instructions)
	if err != nil {
		return err
	}

	if len(run.Proposals) == 0 {
		fmt.Printf("run %s: no changes proposed (nothing to consolidate)\n", run.RunID)
		return nil
	}
	fmt.Printf("run %s: %d proposal(s) written to %s\n\n", run.RunID, len(run.Proposals),
		distilator.RunPath(run.RunID, distilator.ProposalFile))
	for _, p := range run.Proposals {
		fmt.Printf("  %s  (prevalence %.2f, evidence: %s)\n    %s\n",
			p.Path, p.Prevalence, strings.Join(p.Evidence, ", "), p.Rationale)
	}
	fmt.Printf("\nReview, then promote the ones you approve:\n"+
		"  silo-distil promote --project=%s --run-id=%s --paths=<comma-separated>\n", sf.project, run.RunID)
	return nil
}

func show(args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	sf := addStoreFlags(fs)
	runID := fs.String("run-id", "", "run ID to display (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runID == "" {
		return fmt.Errorf("--run-id is required")
	}

	_, store, closeFn, err := sf.open()
	if err != nil {
		return err
	}
	defer closeFn()

	run, err := distilator.NewReviewer(store, nil).LoadRun(context.Background(), sf.project, *runID)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func promote(args []string) error {
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	sf := addStoreFlags(fs)
	runID := fs.String("run-id", "", "run ID to promote from (required)")
	paths := fs.String("paths", "", "comma-separated proposal paths to promote (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runID == "" || *paths == "" {
		return fmt.Errorf("--run-id and --paths are required")
	}

	d, store, closeFn, err := sf.open()
	if err != nil {
		return err
	}
	defer closeFn()

	var approved []string
	for _, p := range strings.Split(*paths, ",") {
		if p = strings.TrimSpace(p); p != "" {
			approved = append(approved, p)
		}
	}

	promoted, err := distilator.NewReviewer(store, d).Promote(context.Background(), sf.project, *runID, approved)
	// Report what landed even on failure — a partial promotion must be visible.
	for _, p := range promoted {
		fmt.Printf("promoted %s (from run %s)\n", p, *runID)
	}
	return err
}
