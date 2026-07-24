// Command siloctl is the admin CLI: project onboarding (one automated command)
// and the deliberately-manual, per-layer teardown flow.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/tooltropolis/silo/internal/admin"
	"github.com/tooltropolis/silo/internal/backend"
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

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tSTATUS\tBUCKET\tNEXT TEARDOWN STEP")
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", rec.ProjectID, rec.Status, bucket, pending)
	}
	return w.Flush()
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

	o := &admin.Onboarder{Registry: reg, KMS: km, Backend: be, Creds: creds}
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

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
