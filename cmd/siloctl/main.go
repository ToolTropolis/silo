// Command siloctl is the admin CLI: project onboarding (one automated command)
// and the deliberately-manual, per-layer teardown flow.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

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
		fmt.Fprintln(os.Stderr, "siloctl teardown: not yet implemented (build sequence step 7)")
		os.Exit(1)
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
  siloctl teardown ...                     (not yet implemented)

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

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
