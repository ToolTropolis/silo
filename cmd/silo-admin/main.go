// Command silo-admin is the operator console: cache retention policy and
// project onboarding/teardown.
//
// Deliberately a separate binary from silo-dashboard. The dashboard is
// read-only by design and runs as the restricted silo-runtime identity; this
// console writes fleet configuration and needs the S3 *admin* credential to
// create and destroy buckets. Merging them would undo that credential split.
//
// Authorization is the listener. The default is a Unix socket, where the
// filesystem permissions are the check. Binding TCP requires --admin-token, and
// binding a non-loopback address requires an explicit override on top of that.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tooltropolis/silo/internal/admin"
	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/daemon"
	"github.com/tooltropolis/silo/internal/kms"
	"github.com/tooltropolis/silo/internal/registry"
	webadmin "github.com/tooltropolis/silo/web/admin"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "silo-admin:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("silo-admin", flag.ContinueOnError)
	listen := fs.String("listen", "./data/silo-admin.sock",
		"listen address: a filesystem path for a Unix socket (recommended), or host:port")
	token := fs.String("admin-token", os.Getenv("SILO_ADMIN_TOKEN"),
		"bearer token required on every request (or SILO_ADMIN_TOKEN). Mandatory for a TCP listener")
	allowRemote := fs.Bool("allow-remote", false,
		"permit binding a non-loopback address. Off by default: this surface can destroy a project")
	rqliteAddrs := fs.String("rqlite", "http://localhost:4001", "comma-separated rqlite node addresses")
	daemonAddr := fs.String("daemon", os.Getenv("SILO_DAEMON_ADMIN_ADDR"),
		"silod's admin socket path or host:port (or SILO_DAEMON_ADMIN_ADDR), for cache stats and maintenance")
	backendEndpoint := fs.String("backend-endpoint", "http://localhost:8333", "SeaweedFS S3 endpoint")
	backendRegion := fs.String("backend-region", "us-east-1", "S3 region")
	s3AccessKey := fs.String("s3-access-key", os.Getenv("SILO_S3_ACCESS_KEY"),
		"S3 ADMIN access key, for bucket lifecycle (or SILO_S3_ACCESS_KEY)")
	s3SecretKey := fs.String("s3-secret-key", os.Getenv("SILO_S3_SECRET_KEY"),
		"S3 ADMIN secret key (or SILO_S3_SECRET_KEY)")
	vaultAddr := fs.String("vault", "http://localhost:8201", "Vault address")
	vaultToken := fs.String("vault-token", os.Getenv("VAULT_TOKEN"), "Vault token (or VAULT_TOKEN)")
	weedBinary := fs.String("weed-binary", "weed", "path to the weed binary, for issuing scoped credentials")
	weedFiler := fs.String("weed-filer", "localhost:8888", "SeaweedFS filer address")
	weedMaster := fs.String("weed-master", "localhost:9333", "SeaweedFS master address")
	agentDaemon := fs.String("agent-daemon", "http://127.0.0.1:8500",
		"the daemon address written into a repo's .mcp.json — the address an AGENT reaches, which is not --daemon (this console's admin socket)")
	mcpBinary := fs.String("mcp-binary", "",
		"the command written into .mcp.json (default: silo-mcp resolved to an absolute path, else the bare name)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Resolve to an absolute path so the agent does not need silo-mcp on its
	// PATH. An agent launched from a GUI or a different shell rarely inherits
	// the PATH the operator installed into, and a bare name fails there with a
	// spawn error that says nothing about PATH.
	if *mcpBinary == "" {
		*mcpBinary = resolveMCPBinary()
	}

	if err := checkListenSafety(*listen, *token, *allowRemote); err != nil {
		return err
	}

	ctx := context.Background()

	reg, err := registry.NewRqlite(ctx, splitCSV(*rqliteAddrs))
	if err != nil {
		return fmt.Errorf("connect registry: %w", err)
	}
	defer reg.Close()

	cfg := webadmin.Config{
		Registry:        reg,
		Settings:        reg,
		Tokens:          reg,
		Token:           *token,
		AgentDaemonAddr: *agentDaemon,
		MCPBinary:       *mcpBinary,
	}

	// The daemon is optional: without it the console still shows and edits
	// policy, and says plainly that live sizes and maintenance actions are
	// unavailable rather than rendering zeros.
	if *daemonAddr != "" {
		cfg.Daemon = webadmin.NewDaemonClient(*daemonAddr)
	}

	// Provisioning is optional too, and needs the admin S3 credential. Without
	// it the console is a policy and observability surface only, which is a
	// reasonable way to run it.
	if *s3AccessKey != "" && *s3SecretKey != "" {
		prov, err := newProvisioner(ctx, provisionerConfig{
			registry:        reg,
			settings:        reg,
			tokens:          reg,
			backendEndpoint: *backendEndpoint,
			backendRegion:   *backendRegion,
			accessKey:       *s3AccessKey,
			secretKey:       *s3SecretKey,
			vaultAddr:       *vaultAddr,
			vaultToken:      *vaultToken,
			weedBinary:      *weedBinary,
			weedFiler:       *weedFiler,
			weedMaster:      *weedMaster,
			daemonAddr:      *daemonAddr,
		})
		if err != nil {
			return err
		}
		cfg.Prov = prov
		// Probes power the wizard's preflight step, which turns most
		// provisioning failures into a message shown before anything is created.
		cfg.BackendProbe = prov.BackendProbe()
		cfg.CredsProbe = prov.CredsProbe()
	} else {
		fmt.Println("silo-admin: no S3 admin credentials; onboarding and teardown are disabled")
	}

	srv, err := webadmin.NewServer(cfg)
	if err != nil {
		return err
	}

	ln, err := daemon.Listen(*listen)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if isSocketPath(*listen) {
		fmt.Printf("silo-admin: listening on unix socket %s (mode 0700)\n", *listen)
		fmt.Printf("silo-admin: browse it with: ssh -L 8700:%s <host>  # or: curl --unix-socket %s http://localhost/\n",
			*listen, *listen)
	} else {
		fmt.Printf("silo-admin: http://%s (token required)\n", *listen)
	}
	return httpSrv.Serve(ln)
}

// checkListenSafety refuses the configurations that would expose a
// project-destroying surface without authentication.
//
// A Unix socket needs no token: the filesystem permissions are the boundary,
// and daemon.Listen chmods it 0700. TCP has no such boundary, so a token is
// mandatory, and a non-loopback bind needs a second, explicit opt-in — an
// operator who types 0.0.0.0 by accident should not publish teardown to the
// network.
func checkListenSafety(listen, token string, allowRemote bool) error {
	if isSocketPath(listen) {
		return nil
	}
	if token == "" {
		return fmt.Errorf("--listen %s is a TCP address, so --admin-token is required "+
			"(this surface can delete a project's bucket). Use a Unix socket path instead "+
			"to authorize by filesystem permissions", listen)
	}

	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("bad --listen %q: %w", listen, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		if !allowRemote {
			return fmt.Errorf("--listen %s binds every interface, which publishes onboarding "+
				"and teardown to the network. Bind 127.0.0.1 instead, or pass --allow-remote "+
				"if that is genuinely intended", listen)
		}
		fmt.Println("silo-admin: WARNING bound to all interfaces; the admin token is the only thing protecting teardown")
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() && !allowRemote {
		return fmt.Errorf("--listen %s is not a loopback address. Pass --allow-remote if "+
			"exposing onboarding and teardown beyond this host is intended", listen)
	}
	return nil
}

// isSocketPath reports whether addr names a filesystem path rather than a TCP
// address, matching daemon.Listen's own rule so the two cannot disagree about
// which kind of listener is being created.
func isSocketPath(addr string) bool { return !strings.Contains(addr, ":") }

type provisionerConfig struct {
	registry                          registry.TenantRegistry
	settings                          admin.SettingsRemover
	tokens                            admin.TokenRevoker
	backendEndpoint, backendRegion    string
	accessKey, secretKey              string
	vaultAddr, vaultToken             string
	weedBinary, weedFiler, weedMaster string
	daemonAddr                        string
}

// newProvisioner wires the same Onboarder siloctl uses, so the console cannot
// drift from the CLI on what onboarding or a teardown step means.
func newProvisioner(ctx context.Context, c provisionerConfig) (*webadmin.OnboarderProvisioner, error) {
	km, err := kms.NewVault(ctx, kms.Config{Address: c.vaultAddr, Token: c.vaultToken})
	if err != nil {
		return nil, fmt.Errorf("connect vault: %w", err)
	}
	be, err := backend.NewSeaweedFS(backend.Config{
		Endpoint:  c.backendEndpoint,
		Region:    c.backendRegion,
		AccessKey: c.accessKey,
		SecretKey: c.secretKey,
	})
	if err != nil {
		return nil, fmt.Errorf("connect backend: %w", err)
	}
	creds := admin.NewSeaweedCredentialIssuer(admin.SeaweedConfig{
		WeedBinary: c.weedBinary,
		Filer:      c.weedFiler,
		Master:     c.weedMaster,
	}, km)

	o := &admin.Onboarder{
		Registry: c.registry,
		KMS:      km,
		Backend:  be,
		Creds:    creds,
		Settings: c.settings,
		Tokens:   c.tokens,
	}
	// Teardown purges the local cache before destroying the bucket, and that
	// purge only reaches the right cache directory through the daemon. Without
	// a daemon configured the Onboarder warns rather than failing, matching
	// siloctl.
	if c.daemonAddr != "" {
		o.Cache = &daemonPurger{client: webadmin.NewDaemonClient(c.daemonAddr)}
	}
	return &webadmin.OnboarderProvisioner{Onboarder: o, Registry: c.registry}, nil
}

// daemonPurger adapts the console's daemon client to the CachePurger seam that
// teardown depends on, turning a refusal back into an error so the teardown
// step stops rather than proceeding to destroy the bucket.
type daemonPurger struct {
	client *webadmin.DaemonClient
}

func (p *daemonPurger) PurgeCache(ctx context.Context, projectID string) error {
	res, err := p.client.PurgeCache(ctx, projectID)
	if err != nil {
		return err
	}
	if !res.Purged {
		return fmt.Errorf("refused: %s", res.Reason)
	}
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

// resolveMCPBinary finds silo-mcp and returns an absolute path, or the bare
// name when it cannot be found.
//
// Written into .mcp.json, so an agent launched from a GUI or another shell —
// which rarely inherits the operator's PATH — still starts it. Prefers a copy
// beside this binary, since the two ship together.
func resolveMCPBinary() string {
	const name = "silo-mcp"

	if self, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(self), name)
		if info, err := os.Stat(beside); err == nil && !info.IsDir() {
			return beside
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return path
	}
	// Not found: emit the bare name so .mcp.json is still valid, and say so —
	// the agent will fail at launch otherwise, with an error that does not
	// mention PATH.
	fmt.Printf("silo-admin: WARNING %s not found beside this binary or on PATH.\n"+
		"           .mcp.json will use the bare name, so the agent needs it on ITS PATH.\n"+
		"           Fix with: go install ./cmd/%s   (or pass --mcp-binary=/abs/path)\n", name, name)
	return name
}
