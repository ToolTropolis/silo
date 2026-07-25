// Command siloctl is the admin CLI: project onboarding (one automated command)
// and the deliberately-manual, per-layer teardown flow.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	urlpkg "net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tooltropolis/silo/internal/admin"
	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/devstack"
	"github.com/tooltropolis/silo/internal/kms"
	"github.com/tooltropolis/silo/internal/registry"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "onboard":
		if err := runOnboard(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "siloctl onboard:", err)
			os.Exit(1)
		}
	case "teardown":
		if err := runTeardown(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "siloctl teardown:", err)
			os.Exit(1)
		}
	case "status":
		if err := runStatus(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "siloctl status:", err)
			os.Exit(1)
		}
	case "flush":
		if err := runFlush(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "siloctl flush:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "siloctl: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `siloctl — Silo admin CLI

Usage:
  siloctl onboard --project=<id> [flags]   provision a project (registry + key + bucket + credential)
  siloctl status [--rqlite=<addrs>]        list projects and any pending teardown step
  siloctl flush --project=<id> --daemon=<addr> --tokens=<t=p>   force-sync queued writes
  siloctl teardown --project=<id> --step=<step>   decommission one layer (confirmed)

Teardown is deliberately manual: one confirmed step per invocation, in order.
  1. --step=revoke-credential   revoke the project's scoped S3 credential
  2. --step=revoke-key          destroy its per-project SSE key
  3. --step=delete-bucket       DELETE the bucket and every memory version (irreversible)
  4. --step=deregister          remove the registry record

There is no flag that runs all four. Steps are enforced in order against the
registry, so a step cannot run early or twice; "siloctl status" shows what is due.

Run "siloctl onboard -h" for onboarding flags.`)
}

func runOnboard(args []string) error {
	fs := flag.NewFlagSet("onboard", flag.ContinueOnError)
	project := fs.String("project", "", "project ID to onboard (required)")
	backendEndpoint := fs.String("backend-endpoint", "http://localhost:8333", "SeaweedFS S3 endpoint")
	backendRegion := fs.String("backend-region", "us-east-1", "S3 region (SeaweedFS ignores it)")
	s3AccessKey := fs.String("s3-access-key", os.Getenv("SILO_S3_ACCESS_KEY"), "S3 admin access key (or SILO_S3_ACCESS_KEY)")
	s3SecretKey := fs.String("s3-secret-key", os.Getenv("SILO_S3_SECRET_KEY"), "S3 admin secret key (or SILO_S3_SECRET_KEY)")
	rqliteAddrs := fs.String("rqlite", "http://localhost:4001", "comma-separated rqlite node addresses")
	vaultAddr := fs.String("vault", "http://localhost:8201", "Vault address")
	vaultToken := fs.String("vault-token", os.Getenv("VAULT_TOKEN"), "Vault token (or VAULT_TOKEN env)")
	weedBinary := fs.String("weed-binary", "weed", "path to the weed binary used to issue scoped credentials")
	weedFiler := fs.String("weed-filer", "localhost:8888", "SeaweedFS filer host:port for credential issuance")
	weedMaster := fs.String("weed-master", "localhost:9333", "SeaweedFS master host:port for credential issuance")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" {
		return fmt.Errorf("--project is required")
	}
	applyDevDefaults(*backendEndpoint, *rqliteAddrs, *vaultAddr,
		vaultToken, s3AccessKey, s3SecretKey, weedBinary)

	if *vaultToken == "" {
		return fmt.Errorf("Vault token required (--vault-token or VAULT_TOKEN)")
	}
	if *s3AccessKey == "" || *s3SecretKey == "" {
		return fmt.Errorf("S3 admin credentials required (--s3-access-key/--s3-secret-key or " +
			"SILO_S3_ACCESS_KEY/SILO_S3_SECRET_KEY): onboarding creates buckets, and anonymous " +
			"access is disabled once any identity exists. For the dev stack, run deploy/bootstrap-dev.sh")
	}

	ctx := context.Background()

	reg, err := registry.NewRqlite(ctx, splitCSV(*rqliteAddrs))
	if err != nil {
		return fmt.Errorf("connect registry: %w", err)
	}
	defer reg.Close()

	km, err := kms.NewVault(ctx, kms.Config{Address: *vaultAddr, Token: *vaultToken})
	if err != nil {
		return fmt.Errorf("connect kms: %w", err)
	}

	// Onboarding creates buckets, so it authenticates as the S3 admin identity.
	// Anonymous access is disabled cluster-wide once any identity exists (the
	// isolation guarantee), so these credentials are required, not optional.
	be, err := backend.NewSeaweedFS(backend.Config{
		Endpoint:  *backendEndpoint,
		Region:    *backendRegion,
		AccessKey: *s3AccessKey,
		SecretKey: *s3SecretKey,
	})
	if err != nil {
		return fmt.Errorf("connect backend: %w", err)
	}

	// Real bucket-scoped credential issuer: creates a per-project SeaweedFS
	// identity via `weed shell` s3.configure, scoped Read,Write on that
	// project's bucket only — the enforced isolation boundary. The secret key
	// is stored in Vault (km), never on the registry.
	creds := admin.NewSeaweedCredentialIssuer(admin.SeaweedConfig{
		WeedBinary: *weedBinary,
		Filer:      *weedFiler,
		Master:     *weedMaster,
	}, km)

	o := &admin.Onboarder{
		Registry: reg,
		KMS:      km,
		Backend:  be,
		Creds:    creds,
	}
	if err := o.Onboard(ctx, *project); err != nil {
		return err
	}
	fmt.Printf("onboarded project %q: bucket, per-project key, registry record, and credential provisioned.\n", *project)
	return nil
}

// runStatus lists projects and, for any mid-teardown, what step is due next.
//
// Without this, inspecting teardown state meant hand-writing rqlite queries —
// exactly when you least want to improvise, since a stalled teardown may have
// left a live bucket behind.
func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	rqliteAddrs := fs.String("rqlite", "http://localhost:4001", "comma-separated rqlite node addresses")
	daemonAddr := fs.String("daemon", os.Getenv("SILO_DAEMON_ADDR"), "daemon address, to report unsynced writes (or SILO_DAEMON_ADDR)")
	daemonTokens := fs.String("tokens", os.Getenv("SILO_TOKENS"), "comma-separated token=projectID pairs for querying the daemon (or SILO_TOKENS)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	reg, err := registry.NewRqlite(ctx, splitCSV(*rqliteAddrs))
	if err != nil {
		return fmt.Errorf("connect registry: %w", err)
	}
	defer reg.Close()

	recs, err := reg.List(ctx)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	if len(recs) == 0 {
		fmt.Println("No projects registered.")
		return nil
	}

	// Queue depths come from the daemon, which owns the bbolt files — siloctl
	// must not open them itself, since a second process fighting the daemon for
	// the bbolt lock is a good way to hang both.
	queues := fetchQueueDepths(ctx, *daemonAddr, *daemonTokens)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tSTATUS\tBUCKET\tUNSYNCED\tNEXT TEARDOWN STEP")
	for _, rec := range recs {
		bucket := rec.BucketName
		if bucket == "" {
			bucket = "-"
		}
		next, err := admin.NextStep(ctx, reg, rec.ProjectID)
		if err != nil {
			next = "?"
		}
		pending := string(next)
		if pending == "" {
			pending = "-"
		}
		// "?" rather than "0" when the daemon wasn't reachable: claiming zero
		// unsynced writes without having checked is exactly the false assurance
		// this column exists to remove.
		unsynced := "?"
		if q, ok := queues[rec.ProjectID]; ok {
			unsynced = strconv.Itoa(q)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", rec.ProjectID, rec.Status, bucket, unsynced, pending)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if *daemonAddr == "" || *daemonTokens == "" {
		fmt.Println("\nUNSYNCED is \"?\" — pass --daemon and --tokens to report writes still")
		fmt.Println("buffered on the daemon's disk, e.g.")
		fmt.Println("  siloctl status --daemon=http://127.0.0.1:8500 --tokens \"tok=proj\"")
	}
	return nil
}

// fetchQueueDepths asks the daemon how many writes are still unsynced per
// project.
//
// One request per token, because the daemon deliberately scopes /v1/queue to the
// caller's own project — there is no endpoint that reports every project's queue
// behind a single agent token, and adding one would put fleet-wide state behind
// an agent credential.
//
// Failures are silent by design: a project simply keeps its "?" and the rest of
// status still prints. This is the command you run when something is broken, so
// it must not fail just because the daemon is one of the broken things.
func fetchQueueDepths(ctx context.Context, addr, tokens string) map[string]int {
	out := map[string]int{}
	if addr == "" || tokens == "" {
		return out
	}

	client := &http.Client{Timeout: 5 * time.Second}
	for _, pair := range splitCSV(tokens) {
		token, projectID, ok := strings.Cut(pair, "=")
		if !ok || token == "" || projectID == "" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(addr, "/")+"/v1/queue", nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var body struct {
			Project string `json:"project"`
			Pending int    `json:"pending"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if decodeErr != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		// Trust the daemon's answer about which project it reported on, not the
		// projectID in our own flag — they should agree, and if they don't the
		// daemon is authoritative about its own queue.
		if body.Project != "" {
			out[body.Project] = body.Pending
		}
	}
	return out
}

// runTeardown performs ONE confirmed teardown layer.
//
// There is deliberately no flag that runs every step: teardown is manual and
// per-layer by design (spec §5). Each invocation prompts for confirmation, and
// the irreversible step requires typing the project ID rather than "yes".
func runTeardown(args []string) error {
	fs := flag.NewFlagSet("teardown", flag.ContinueOnError)
	project := fs.String("project", "", "project ID to tear down (required)")
	stepName := fs.String("step", "", "one of: revoke-credential, revoke-key, delete-bucket, deregister (required)")
	daemonAddr := fs.String("daemon", os.Getenv("SILO_DAEMON_ADDR"), "daemon address, to check for unsynced writes before deleting a bucket (or SILO_DAEMON_ADDR)")
	daemonTokens := fs.String("tokens", os.Getenv("SILO_TOKENS"), "comma-separated token=projectID pairs for the daemon check (or SILO_TOKENS)")
	adminSocket := fs.String("admin-socket", os.Getenv("SILO_ADMIN_SOCKET"), "daemon admin socket, used to purge the local cache when the bucket is deleted (or SILO_ADMIN_SOCKET)")
	assumeYes := fs.Bool("yes", false, "skip the interactive prompt (for scripted use); still one step per invocation")
	backendEndpoint := fs.String("backend-endpoint", "http://localhost:8333", "SeaweedFS S3 endpoint")
	backendRegion := fs.String("backend-region", "us-east-1", "S3 region (SeaweedFS ignores it)")
	s3AccessKey := fs.String("s3-access-key", os.Getenv("SILO_S3_ACCESS_KEY"), "S3 admin access key (or SILO_S3_ACCESS_KEY)")
	s3SecretKey := fs.String("s3-secret-key", os.Getenv("SILO_S3_SECRET_KEY"), "S3 admin secret key (or SILO_S3_SECRET_KEY)")
	rqliteAddrs := fs.String("rqlite", "http://localhost:4001", "comma-separated rqlite node addresses")
	vaultAddr := fs.String("vault", "http://localhost:8201", "Vault address")
	vaultToken := fs.String("vault-token", os.Getenv("VAULT_TOKEN"), "Vault token (or VAULT_TOKEN env)")
	weedBinary := fs.String("weed-binary", "weed", "path to the weed binary used to revoke scoped credentials")
	weedFiler := fs.String("weed-filer", "localhost:8888", "SeaweedFS filer host:port")
	weedMaster := fs.String("weed-master", "localhost:9333", "SeaweedFS master host:port")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *project == "" {
		return fmt.Errorf("--project is required")
	}
	if *stepName == "" {
		return fmt.Errorf("--step is required: teardown runs one confirmed layer at a time " +
			"(revoke-credential → revoke-key → delete-bucket → deregister)")
	}
	step, err := admin.ParseStep(*stepName)
	if err != nil {
		return err
	}
	applyDevDefaults(*backendEndpoint, *rqliteAddrs, *vaultAddr,
		vaultToken, s3AccessKey, s3SecretKey, weedBinary)

	if *vaultToken == "" {
		return fmt.Errorf("Vault token required (--vault-token or VAULT_TOKEN)")
	}
	if *s3AccessKey == "" || *s3SecretKey == "" {
		return fmt.Errorf("S3 admin credentials required (--s3-access-key/--s3-secret-key or " +
			"SILO_S3_ACCESS_KEY/SILO_S3_SECRET_KEY)")
	}

	ctx := context.Background()

	reg, err := registry.NewRqlite(ctx, splitCSV(*rqliteAddrs))
	if err != nil {
		return fmt.Errorf("connect registry: %w", err)
	}
	defer reg.Close()

	// Check ordering BEFORE prompting. Teardown itself re-checks — that's the
	// real guard — but asking someone to type a project ID to authorize an
	// irreversible delete, then refusing the step anyway, trains them to type it
	// reflexively. Refuse first, prompt only for a step that will actually run.
	if due, err := admin.NextStep(ctx, reg, *project); err == nil && due != step {
		if due == "" {
			return fmt.Errorf("project %q is already fully decommissioned", *project)
		}
		return fmt.Errorf("project %q is not ready for %q; next step is %q\n"+
			"  run: siloctl teardown --project=%s --step=%s", *project, step, due, *project, due)
	}

	// Refuse to delete a bucket while writes for it are still buffered on a
	// daemon's disk. Those writes are addressed to a bucket that is about to
	// stop existing, so the drain would fail forever and the data is simply
	// gone — unrecoverable, and invisible unless someone thought to look.
	if step == admin.StepDeleteBucket {
		token := tokenFor(*daemonTokens, *project)
		switch depth := queueDepthFor(ctx, *daemonAddr, token); {
		case depth > 0:
			return fmt.Errorf("project %q has %d write(s) still queued on the daemon\n"+
				"  Deleting the bucket now would discard them permanently.\n"+
				"  Drain them first: siloctl flush --project=%s --daemon=%s --tokens=%q",
				*project, depth, *project, *daemonAddr, *daemonTokens)
		case depth < 0 && *daemonAddr != "":
			// Configured but unreachable — that is a real uncertainty, not a
			// clean bill of health.
			fmt.Printf("  WARNING: could not reach the daemon to check for unsynced writes.\n")
		case depth < 0:
			fmt.Printf("  NOTE: no --daemon given, so unsynced writes were not checked.\n" +
				"        Pass --daemon and --tokens to verify nothing is still queued.\n")
		}
	}

	if err := confirmStep(os.Stdin, os.Stdout, *project, step, *assumeYes); err != nil {
		return err
	}

	km, err := kms.NewVault(ctx, kms.Config{Address: *vaultAddr, Token: *vaultToken})
	if err != nil {
		return fmt.Errorf("connect KMS: %w", err)
	}

	be, err := backend.NewSeaweedFS(backend.Config{
		Endpoint:  *backendEndpoint,
		Region:    *backendRegion,
		AccessKey: *s3AccessKey,
		SecretKey: *s3SecretKey,
	})
	if err != nil {
		return fmt.Errorf("connect backend: %w", err)
	}

	creds := admin.NewSeaweedCredentialIssuer(admin.SeaweedConfig{
		WeedBinary: *weedBinary,
		Filer:      *weedFiler,
		Master:     *weedMaster,
	}, km)

	o := &admin.Onboarder{Registry: reg, KMS: km, Backend: be, Creds: creds, Settings: reg}
	if *adminSocket != "" {
		o.Cache = newDaemonCachePurger(*adminSocket)
	}
	if err := o.Teardown(ctx, *project, step); err != nil {
		return err
	}

	fmt.Printf("teardown step %q completed for project %q.\n", step, *project)

	// Ask the registry what's actually left rather than assuming the next step
	// positionally. "Fully decommissioned" must mean the record is gone, not
	// merely that the last-named step happened to run.
	next, err := admin.NextStep(ctx, reg, *project)
	if err != nil {
		// The work succeeded; only the follow-up hint is unavailable.
		fmt.Printf("(could not determine the next step: %v)\n", err)
		return nil
	}
	if next != "" {
		fmt.Printf("Next: siloctl teardown --project=%s --step=%s\n", *project, next)
	} else {
		fmt.Printf("Project %q is fully decommissioned.\n", *project)
	}
	return nil
}

// confirmStep gates a destructive action on explicit human input.
//
// The irreversible step demands the project ID be typed out — a reflexive "y"
// shouldn't be able to delete a bucket. --yes bypasses the prompt for scripted
// use, but never bypasses the one-step-per-invocation rule.
func confirmStep(in io.Reader, out io.Writer, project string, step admin.TeardownStep, assumeYes bool) error {
	if assumeYes {
		return nil
	}

	if admin.IsIrreversible(step) {
		fmt.Fprintf(out, "\n  IRREVERSIBLE: this deletes project %q's bucket and every version of its memory.\n", project)
		fmt.Fprintf(out, "  This cannot be undone.\n\n")
		fmt.Fprintf(out, "  Type the project ID to confirm: ")
		typed, err := readLine(in)
		if err != nil {
			return fmt.Errorf("read confirmation: %w", err)
		}
		if typed != project {
			return fmt.Errorf("confirmation did not match (got %q, want %q) — aborted", typed, project)
		}
		return nil
	}

	fmt.Fprintf(out, "\n  Run teardown step %q for project %q? [y/N]: ", step, project)
	answer, err := readLine(in)
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	switch strings.ToLower(answer) {
	case "y", "yes":
		return nil
	default:
		return fmt.Errorf("aborted")
	}
}

func readLine(in io.Reader) (string, error) {
	r := bufio.NewReader(in)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// applyDevDefaults fills unset credentials with the local dev-stack values, but
// only when every endpoint is loopback.
//
// The gate is the point. Defaulting a Vault token unconditionally would put
// "dev-only-token" in a released binary, where it is convenient enough that
// nobody notices it authenticating against something real. Talking to a
// non-local endpoint therefore still requires stating credentials explicitly.
//
// Only empty values are filled, so an explicit flag always wins.
func applyDevDefaults(backendEndpoint, rqliteAddrs, vaultAddr string,
	vaultToken, s3AccessKey, s3SecretKey, weedBinary *string) {

	if !devstack.IsLocal(backendEndpoint, rqliteAddrs, vaultAddr) {
		return
	}
	if *vaultToken == "" {
		*vaultToken = devstack.VaultToken
	}
	if *s3AccessKey == "" {
		*s3AccessKey = devstack.AdminKey
	}
	if *s3SecretKey == "" {
		*s3SecretKey = devstack.AdminSecret
	}
	// "weed" is the flag's own default and only resolves inside the container,
	// so on the dev stack it is never the right value.
	if *weedBinary == "" || *weedBinary == "weed" {
		*weedBinary = devstack.WeedBinary
	}
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

// runFlush forces a project's buffered writes to the backend and blocks until
// it knows the outcome.
//
// The sync worker gets there on its own schedule; this is for the moments when
// "eventually" is not an acceptable answer — before stopping a host, or before
// tearing a project down. It exits non-zero if anything is still queued, so it
// composes into a gate:
//
//	siloctl flush --project=X ... && siloctl teardown --project=X --step=...
func runFlush(args []string) error {
	fs := flag.NewFlagSet("flush", flag.ContinueOnError)
	projectID := fs.String("project", "", "project whose queued writes to flush (required)")
	daemonAddr := fs.String("daemon", os.Getenv("SILO_DAEMON_ADDR"), "daemon address (or SILO_DAEMON_ADDR)")
	tokens := fs.String("tokens", os.Getenv("SILO_TOKENS"), "comma-separated token=projectID pairs (or SILO_TOKENS)")
	timeout := fs.Duration("timeout", 60*time.Second, "how long to wait for the drain")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *projectID == "" {
		return fmt.Errorf("--project is required")
	}
	if *daemonAddr == "" {
		return fmt.Errorf("--daemon is required: the daemon owns the queue, so only it can drain one")
	}

	token := tokenFor(*tokens, *projectID)
	if token == "" {
		return fmt.Errorf("no token for project %q in --tokens", *projectID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	res, err := flushProject(ctx, *daemonAddr, token)
	if err != nil {
		return err
	}

	fmt.Printf("flushed %q: %d write(s) drained, %d remaining\n", *projectID, res.Drained, res.Remaining)
	if res.Error != "" {
		fmt.Printf("  the drain reported: %s\n", res.Error)
	}
	if res.Remaining > 0 {
		return fmt.Errorf("%d write(s) are still queued — the backend is not accepting them, "+
			"so this project is NOT safe to tear down", res.Remaining)
	}
	return nil
}

// syncResult mirrors the daemon's /v1/sync response.
type syncResult struct {
	Project   string `json:"project"`
	Drained   int    `json:"drained"`
	Remaining int    `json:"remaining"`
	Error     string `json:"error,omitempty"`
}

// flushProject asks the daemon to drain now.
func flushProject(ctx context.Context, addr, token string) (syncResult, error) {
	var out syncResult
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(addr, "/")+"/v1/sync", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return out, fmt.Errorf("contact daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("daemon returned %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

// tokenFor finds the token scoped to a project in a token=projectID list.
func tokenFor(spec, projectID string) string {
	for _, pair := range splitCSV(spec) {
		token, proj, ok := strings.Cut(pair, "=")
		if ok && proj == projectID {
			return token
		}
	}
	return ""
}

// queueDepthFor asks the daemon how many writes are still buffered, for the
// teardown gate. Returns -1 when it cannot tell, which the caller must treat as
// "unknown" rather than "none".
func queueDepthFor(ctx context.Context, addr, token string) int {
	if addr == "" || token == "" {
		return -1
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(addr, "/")+"/v1/queue", nil)
	if err != nil {
		return -1
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -1
	}
	var body struct {
		Pending int `json:"pending"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return -1
	}
	return body.Pending
}

// daemonCachePurger asks a daemon to drop a project's local cache over its
// admin socket.
//
// siloctl never opens the bbolt files itself: the daemon holds an exclusive
// lock on them, so a second process would block for five seconds and then fail
// with a timeout that looks nothing like its actual cause.
type daemonCachePurger struct {
	socket string
	client *http.Client
}

// newDaemonCachePurger dials the daemon's admin socket. A path with no colon is
// a Unix socket, matching the daemon's own listener rule.
func newDaemonCachePurger(socket string) *daemonCachePurger {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &daemonCachePurger{
		socket: socket,
		client: &http.Client{Transport: transport, Timeout: 15 * time.Second},
	}
}

func (p *daemonCachePurger) PurgeCache(ctx context.Context, projectID string) error {
	url := "http://silod/v1/admin/purge-cache?project=" + urlpkg.QueryEscape(projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("contact daemon at %s: %w", p.socket, err)
	}
	defer resp.Body.Close()

	var body struct {
		Purged  bool   `json:"purged"`
		Pending int    `json:"pending"`
		Error   string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)

	switch {
	case resp.StatusCode == http.StatusOK && body.Purged:
		return nil
	case resp.StatusCode == http.StatusConflict:
		return fmt.Errorf("%d write(s) are still queued; drain them first with siloctl flush", body.Pending)
	default:
		if body.Error != "" {
			return fmt.Errorf("daemon refused the purge: %s", body.Error)
		}
		return fmt.Errorf("daemon returned %d", resp.StatusCode)
	}
}
