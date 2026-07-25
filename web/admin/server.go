// Package admin is the operator console: cache retention policy and project
// onboarding/teardown.
//
// Deliberately separate from web/dashboard. The dashboard is read-only by
// design and runs as the restricted silo-runtime identity; this surface writes
// fleet configuration and needs the S3 *admin* credential to create and destroy
// buckets. Merging them would undo that credential split — every dashboard
// viewer would inherit the ability to tear down a project.
//
// Security posture: authorization is the listener. Bound to a Unix socket, the
// filesystem permissions are the check. Bound to TCP, a token is mandatory and
// compared in constant time. Neither path exposes raw credentials or key
// material, because neither is read here.
package admin

import (
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/tooltropolis/silo/internal/registry"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/favicon.ico static/logo.svg static/console.css
var staticFS embed.FS

// Registry is the read-only slice of the tenant registry the console renders.
type Registry interface {
	List(ctx context.Context) ([]registry.ProjectRecord, error)
	Get(ctx context.Context, projectID string) (registry.ProjectRecord, error)
}

// Settings reads and writes cache retention policy.
type Settings interface {
	GetSettings(ctx context.Context, projectID string) (registry.CacheSettings, error)
	ListSettings(ctx context.Context) (map[string]registry.CacheSettings, error)
	PutSettings(ctx context.Context, projectID string, s registry.CacheSettings) error
	DeleteSettings(ctx context.Context, projectID string) error
}

// DaemonAdmin is the daemon's operator socket: live cache stats and the two
// maintenance actions.
//
// The console never opens bbolt itself. A second process contending for the
// same file lock would hang both, and the daemon is the only thing that knows
// which cache directory is live.
type DaemonAdmin interface {
	CacheStats(ctx context.Context) ([]ProjectCacheStat, error)
	PurgeCache(ctx context.Context, projectID string) (PurgeOutcome, error)
	CompactCache(ctx context.Context, projectID string) (CompactOutcome, error)
}

// Provisioner onboards and tears down projects.
//
// Teardown is per-layer and confirmed, mirroring siloctl rather than becoming a
// one-click destroy: the console reuses the same step semantics so an operator
// cannot do anything here they could not do, with the same guardrails, there.
type Provisioner interface {
	Onboard(ctx context.Context, projectID string) error
	TeardownStep(ctx context.Context, projectID, step string) (string, error)
	TeardownPlan(ctx context.Context, projectID string) ([]TeardownStep, error)
}

// Server serves the console.
type Server struct {
	registry Registry
	settings Settings
	daemon   DaemonAdmin
	prov     Provisioner
	token    string
	// backendProbe and credsProbe power the wizard's preflight step. Optional:
	// an absent prober reports "cannot verify" rather than a false pass.
	backendProbe BackendProber
	credsProbe   CredentialProber
	// tokens mints agent tokens for the Connect step. Optional: without it the
	// wizard shows the config to copy but cannot issue a credential.
	tokens TokenMinter
	// vault holds a freshly minted token for its single reveal.
	vault *tokenVault
	// agentDaemonAddr and mcpBinary are what the generated .mcp.json points at.
	agentDaemonAddr string
	mcpBinary       string
	// tracker holds in-flight provisioning progress, so the wizard can show
	// which layer failed rather than only that something did.
	tracker   *provisionTracker
	templates map[string]*template.Template
	mux       *http.ServeMux
}

// Config wires the console's dependencies. Every one is optional: a missing
// dependency makes its view report that it is not configured rather than
// panicking, so the console is useful against a partial deployment.
type Config struct {
	Registry Registry
	Settings Settings
	Daemon   DaemonAdmin
	Prov     Provisioner
	// Token, when set, is required as a bearer token on every request. Mandatory
	// for a TCP listener; unnecessary for a Unix socket, where the filesystem
	// permissions already are the boundary.
	Token string
	// BackendProbe and CredsProbe let the wizard verify onboarding will succeed
	// before it creates anything. Optional.
	BackendProbe BackendProber
	CredsProbe   CredentialProber
	// Tokens issues agent tokens from the Connect step. Optional.
	Tokens TokenMinter
	// AgentDaemonAddr is the daemon address written into a repo's .mcp.json —
	// the address an *agent* uses, which is not the admin socket this console
	// talks to.
	AgentDaemonAddr string
	// MCPBinary is the command name written into .mcp.json.
	MCPBinary string
}

// TokenMinter issues agent tokens. Narrow on purpose: the console mints and
// lists, and teardown revokes, but nothing here needs to verify one.
type TokenMinter interface {
	MintToken(ctx context.Context, projectID, label, createdBy string) (string, error)
	ListTokens(ctx context.Context, projectID string) ([]registry.AgentToken, error)
	RevokeToken(ctx context.Context, hash string) error
}

func NewServer(cfg Config) (*Server, error) {
	views, err := parseViews()
	if err != nil {
		return nil, err
	}
	s := &Server{
		registry:        cfg.Registry,
		settings:        cfg.Settings,
		daemon:          cfg.Daemon,
		prov:            cfg.Prov,
		token:           cfg.Token,
		backendProbe:    cfg.BackendProbe,
		credsProbe:      cfg.CredsProbe,
		tokens:          cfg.Tokens,
		vault:           newTokenVault(),
		agentDaemonAddr: cfg.AgentDaemonAddr,
		mcpBinary:       cfg.MCPBinary,
		tracker:         newProvisionTracker(),
		templates:       views,
		mux:             http.NewServeMux(),
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	// "/" is a catch-all in net/http's mux, so an unknown path would otherwise
	// silently render the cache view. Serve it only at exactly "/".
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		s.handleCache(w, r)
	})
	s.mux.HandleFunc("/settings", s.handleSettings)
	s.mux.HandleFunc("/projects", s.handleProjects)
	s.mux.HandleFunc("/project", s.handleProject)
	s.mux.HandleFunc("/tokens/mint", s.handleMintToken)
	s.mux.HandleFunc("/tokens/revoke", s.handleRevokeToken)
	// The wizard owns /onboard/*; the bare /onboard stays a plain POST so the
	// existing single-form flow and its tests keep working.
	s.mux.HandleFunc("/onboard", s.handleOnboard)
	s.mux.HandleFunc("/onboard/", s.handleWizard)
	s.mux.HandleFunc("/teardown", s.handleTeardown)
	s.mux.HandleFunc("/cache-action", s.handleCacheAction)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.mux.HandleFunc("/favicon.ico", serveAsset("static/favicon.ico", "image/x-icon"))
	s.mux.HandleFunc("/logo.svg", serveAsset("static/logo.svg", "image/svg+xml"))
	s.mux.HandleFunc("/console.css", serveAsset("static/console.css", "text/css; charset=utf-8"))
}

// Handler returns the routed handler with authentication applied.
func (s *Server) Handler() http.Handler { return s.authenticate(s.mux) }

// authenticate enforces the bearer token when one is configured.
//
// Constant-time comparison: a byte-by-byte early return leaks the token's
// prefix to anyone who can time responses, and this surface can destroy a
// project.
func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.token == "" {
		// No token means a Unix socket, where the filesystem is the boundary.
		// silo-admin refuses to start a TCP listener without one, so this is
		// not an open door.
		return next
	}
	want := []byte(s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /healthz stays open so a supervisor can check liveness without being
		// handed a credential that could tear down a project.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		// The scheme is required, not optionally stripped: TrimPrefix is a no-op
		// when the prefix is absent, which would let a bare "Authorization:
		// <token>" through and quietly widen what counts as a valid credential.
		raw := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(raw, "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="silo-admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func serveAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(data)
	}
}

var viewTitles = map[string]string{
	"cache.html":          "Cache",
	"settings.html":       "Settings",
	"projects.html":       "Projects",
	"project.html":        "Project",
	"wizard_name.html":    "Onboard a project",
	"wizard_checks.html":  "Onboard — checks",
	"wizard_review.html":  "Onboard — review",
	"wizard_connect.html": "Onboard — connect a repo",
	"wizard_status.html":  "Onboard — provisioning",
	"wizard_done.html":    "Onboard — done",
	"error.html":          "Error",
}

// contentViews are every page rendered inside the app shell.
var contentViews = []string{
	"cache.html", "settings.html", "projects.html", "project.html",
	"wizard_name.html", "wizard_checks.html", "wizard_review.html", "wizard_connect.html",
	"wizard_status.html", "wizard_done.html",
}

// parseViews builds one template set per view. Each view defines its own "body"
// block, so parsing them into a single set would make the last one win and
// silently render for every route.
func parseViews() (map[string]*template.Template, error) {
	sets := map[string]*template.Template{}
	for _, view := range contentViews {
		t, err := template.New(view).Funcs(templateFuncs()).
			ParseFS(templateFS, "templates/layout.html", "templates/"+view)
		if err != nil {
			return nil, fmt.Errorf("admin: parse %s: %w", view, err)
		}
		sets[view] = t
	}
	errTmpl, err := template.New("error.html").Funcs(templateFuncs()).
		ParseFS(templateFS, "templates/error.html")
	if err != nil {
		return nil, fmt.Errorf("admin: parse error.html: %w", err)
	}
	sets["error.html"] = errTmpl
	return sets, nil
}

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	set, ok := s.templates[name]
	if !ok {
		http.Error(w, "admin: unknown view "+name, http.StatusInternalServerError)
		return
	}
	data["Title"] = viewTitles[name]
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	entry := "layout"
	if name == "error.html" {
		entry = "error.html"
	}
	if err := set.ExecuteTemplate(w, entry, data); err != nil {
		http.Error(w, "admin: render "+name+": "+err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) fail(w http.ResponseWriter, view string, err error) {
	w.WriteHeader(http.StatusInternalServerError)
	s.render(w, "error.html", map[string]any{"View": view, "Error": err.Error()})
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"truncate": func(n int, s string) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "…"
		},
		"bytes":    humanBytes,
		"duration": humanDuration,
		"add":      func(a, b int) int { return a + b },
		// pctOf renders how much of a file is reclaimable, which is the signal
		// for whether compaction is worth running.
		"pctOf": func(part, whole int64) string {
			if whole <= 0 {
				return "—"
			}
			return fmt.Sprintf("%.0f%%", float64(part)/float64(whole)*100)
		},
	}
}

// humanBytes formats a size for an operator scanning a table.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// humanDuration renders a retention TTL. Zero is meaningful here — it is an
// explicit "never cache", not an absence — so it must not print as "unset".
func humanDuration(d time.Duration) string {
	if d == 0 {
		return "0 (never cache)"
	}
	return d.String()
}
