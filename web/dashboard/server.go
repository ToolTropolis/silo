// Package dashboard is the minimal v1 web surface: a read-only tenant registry
// view, a memory version browser, and the one write action anywhere — approving
// or rejecting a pending Distilator proposal. It queries the rqlite cluster over
// its HTTP API and reads SeaweedFS directly; no separate API layer for v1.
package dashboard

import "net/http"

// Server serves the three v1 dashboard views. Not yet implemented — build
// sequence step 6 (docs/architecture.md).
type Server struct {
	mux *http.ServeMux
}

// NewServer constructs the dashboard HTTP server.
func NewServer() *Server {
	return &Server{mux: http.NewServeMux()}
}

// Handler exposes the routed handler for the process to serve.
func (s *Server) Handler() http.Handler { return s.mux }
