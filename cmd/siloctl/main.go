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

	be, err := backend.NewSeaweedFS(backend.Config{Endpoint: *backendEndpoint, Region: *backendRegion})
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
