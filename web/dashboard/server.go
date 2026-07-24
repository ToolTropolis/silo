// Package dashboard is the minimal v1 web surface: a read-only tenant registry
// view, a memory version browser, and the one write action anywhere — approving
// or rejecting a pending Distilator proposal.
//
// Security posture: the registry view renders credential and key *references*
// only. Raw credential secrets and key material never reach a template, because
// they are never read here — the registry stores references, and the KMS is not
// wired into this surface at all.
package dashboard

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"net/http"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/distilator"
	"github.com/tooltropolis/silo/internal/registry"
)

//go:embed templates/*.html
var templateFS embed.FS

// Brand assets are embedded so the binary stays self-contained — a dashboard
// that needs files next to it on disk is one more thing to get wrong at deploy.
//
//go:embed static/favicon.ico static/logo.svg
var staticFS embed.FS

// Registry is the read-only slice of the tenant registry the dashboard needs.
type Registry interface {
	List(ctx context.Context) ([]registry.ProjectRecord, error)
	Get(ctx context.Context, projectID string) (registry.ProjectRecord, error)
}

// MemoryReader browses memory paths and their version history.
type MemoryReader interface {
	ListPaths(ctx context.Context, projectID, prefix string) ([]string, error)
	ListVersions(ctx context.Context, projectID, path string) ([]backend.ObjectVersion, error)
	Get(ctx context.Context, projectID, path, versionID string) ([]byte, backend.ObjectVersion, error)
}

// ProposalReviewer lists, loads, and promotes Distilator runs.
type ProposalReviewer interface {
	ListRuns(ctx context.Context, projectID string) ([]string, error)
	LoadRun(ctx context.Context, projectID, runID string) (*distilator.Run, error)
	Promote(ctx context.Context, projectID, runID string, approvedPaths []string) ([]string, error)
}

// Server serves the three v1 dashboard views.
type Server struct {
	registry Registry
	memory   MemoryReader
	reviewer ProposalReviewer
	// templates holds one parsed set per view — see parseViews.
	templates map[string]*template.Template
	mux       *http.ServeMux
}

// NewServer constructs the dashboard HTTP server. Any dependency may be nil;
// the corresponding view then reports that it isn't configured rather than
// panicking, so the dashboard is useful even against a partial deployment.
func NewServer(reg Registry, mem MemoryReader, rev ProposalReviewer) (*Server, error) {
	// Each view is parsed into its OWN set (layout + that view). Every view
	// defines a block named "body"; parsing them all into one set would make
	// the last one win and silently render for every route.
	views, err := parseViews()
	if err != nil {
		return nil, err
	}
	s := &Server{registry: reg, memory: mem, reviewer: rev, templates: views, mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	// "/" is a catch-all in net/http's mux, so an unknown path would otherwise
	// silently render the registry. Serve the registry only at exactly "/" and
	// 404 everything else — a dashboard that answers 200 for /revoke or
	// /teardown reads as if those actions exist.
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		s.handleRegistry(w, r)
	})
	s.mux.HandleFunc("/memory", s.handleMemory)     // §7.2 memory version browser
	s.mux.HandleFunc("/distilations", s.handleRuns) // §7.3 Distilator review
	s.mux.HandleFunc("/promote", s.handlePromote)   // the one write action
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Brand assets. Long-cached: these change only when the logo does, and
	// browsers request /favicon.ico on every cold load.
	s.mux.HandleFunc("/favicon.ico", serveAsset("static/favicon.ico", "image/x-icon"))
	s.mux.HandleFunc("/logo.svg", serveAsset("static/logo.svg", "image/svg+xml"))
}

// serveAsset returns a handler for one embedded static file.
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

// Handler exposes the routed handler for the process to serve.
func (s *Server) Handler() http.Handler { return s.mux }

// viewTitles is the page title per view.
var viewTitles = map[string]string{
	"registry.html":     "Registry",
	"memory.html":       "Memory",
	"distilations.html": "Distilations",
	"error.html":        "Error",
}

// parseViews builds one template set per view, each containing the shared
// layout plus that view's own "body" block.
func parseViews() (map[string]*template.Template, error) {
	sets := map[string]*template.Template{}
	for _, view := range []string{"registry.html", "memory.html", "distilations.html"} {
		t, err := template.New(view).Funcs(templateFuncs()).
			ParseFS(templateFS, "templates/layout.html", "templates/"+view)
		if err != nil {
			return nil, fmt.Errorf("dashboard: parse %s: %w", view, err)
		}
		sets[view] = t
	}
	// error.html is a standalone page with no layout.
	errTmpl, err := template.New("error.html").Funcs(templateFuncs()).
		ParseFS(templateFS, "templates/error.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: parse error.html: %w", err)
	}
	sets["error.html"] = errTmpl
	return sets, nil
}

// render writes a view, reporting failures rather than emitting a partial page.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	set, ok := s.templates[name]
	if !ok {
		http.Error(w, "dashboard: unknown view "+name, http.StatusInternalServerError)
		return
	}
	if d, isMap := data.(map[string]any); isMap {
		d["Title"] = viewTitles[name]
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The layout is the entry point for the full-page views; error.html is
	// self-contained.
	entry := "layout"
	if name == "error.html" {
		entry = "error.html"
	}
	if err := set.ExecuteTemplate(w, entry, data); err != nil {
		http.Error(w, "dashboard: render "+name+": "+err.Error(), http.StatusInternalServerError)
	}
}

// fail renders an error into the page shell so a broken dependency is visible
// in the UI rather than a bare 500 body.
func (s *Server) fail(w http.ResponseWriter, view string, err error) {
	w.WriteHeader(http.StatusInternalServerError)
	s.render(w, "error.html", map[string]any{"View": view, "Error": err.Error()})
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// truncate keeps long opaque references readable in a table cell.
		"truncate": func(n int, s string) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "…"
		},
		"pct": func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
	}
}
