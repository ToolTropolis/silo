package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
)

// TokenVerifier maps a bearer token presented by an agent to the projectID it
// is scoped to. It's the daemon's authorization boundary: a token resolves to
// exactly one project, so an agent can never address another project's memory
// regardless of what it puts in the request.
type TokenVerifier interface {
	// ProjectFor returns the projectID a token is scoped to, or ErrUnauthorized.
	ProjectFor(token string) (string, error)
}

// ErrUnauthorized is returned by a TokenVerifier for an unknown token.
var ErrUnauthorized = errors.New("daemon: unauthorized")

// StaticTokenVerifier is a simple in-memory token->project map, suitable for
// local/dev use and tests. Production wiring resolves tokens against the
// credential store issued at onboarding.
type StaticTokenVerifier map[string]string

// ProjectFor implements TokenVerifier.
func (s StaticTokenVerifier) ProjectFor(token string) (string, error) {
	if p, ok := s[token]; ok && token != "" {
		return p, nil
	}
	return "", ErrUnauthorized
}

// Projects returns the distinct projects this verifier grants access to, sorted.
//
// Deliberately a method on the concrete type rather than on TokenVerifier: the
// interface is the authorization boundary and should stay minimal, and a future
// database-backed verifier should not be forced to enumerate every tenant.
//
// The sync worker uses this to learn which projects it owns. That is sound while
// tokens are the only way to write — no token means no writes means no queue —
// but it does mean a project whose token was rotated away mid-outage would stop
// being drained. Switch to registry.List once the registry is wired into silod.
func (s StaticTokenVerifier) Projects() []string {
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for _, projectID := range s {
		if _, dup := seen[projectID]; dup {
			continue
		}
		seen[projectID] = struct{}{}
		out = append(out, projectID)
	}
	sort.Strings(out) // stable startup logging
	return out
}

// Server exposes the daemon's Read/Write/List/Search operations over HTTP for
// the pkg/client SDK. It listens on a Unix socket for same-machine agents or a
// TCP address.
//
// Every request is authenticated with a bearer token that resolves to a single
// project; the project is never taken from the request body or path, so a
// caller cannot address memory outside its own silo.
type Server struct {
	daemon   *Daemon
	verifier TokenVerifier
	actor    string // recorded on writes when the caller doesn't name itself
}

// NewServer builds the HTTP surface for a daemon.
func NewServer(d *Daemon, v TokenVerifier) *Server {
	return &Server{daemon: d, verifier: v, actor: "sdk"}
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/read", s.handleRead)
	mux.HandleFunc("/v1/write", s.handleWrite)
	mux.HandleFunc("/v1/list", s.handleList)
	mux.HandleFunc("/v1/search", s.handleSearch)
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// Listen binds addr without serving. A path (no colon) is treated as a Unix
// socket; anything else as a TCP address.
//
// Separate from Serve so a caller can report "listening on X" only after the
// bind actually succeeded. Printing before the bind makes a port conflict look
// like a successful start, and the next symptom — every request failing
// against whatever process already owns the port — is baffling.
func (s *Server) Listen(addr string) (net.Listener, error) {
	network := "tcp"
	if !strings.Contains(addr, ":") {
		network = "unix"
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen %s %s: %w", network, addr, err)
	}
	return ln, nil
}

// Serve handles requests on an already-bound listener.
func (s *Server) Serve(ln net.Listener) error {
	return http.Serve(ln, s.Handler())
}

// ListenAndServe binds and serves in one call.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := s.Listen(addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// authorize resolves the request's bearer token to its project scope.
func (s *Server) authorize(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == auth || token == "" { // missing or malformed
		return "", ErrUnauthorized
	}
	return s.verifier.ProjectFor(token)
}

type writeRequest struct {
	Path      string `json:"path"`
	Content   []byte `json:"content"` // base64 in JSON
	Actor     string `json:"actor,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type readResponse struct {
	Content []byte `json:"content"`
}

type listResponse struct {
	Paths []string `json:"paths"`
}

type searchResponse struct {
	Results []SearchHit `json:"results"`
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	projectID, err := s.authorize(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, errors.New("path required"))
		return
	}
	content, err := s.daemon.Read(r.Context(), projectID, path)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, readResponse{Content: content})
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	projectID, err := s.authorize(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return
	}
	var req writeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad body: %w", err))
		return
	}
	if req.Path == "" {
		writeErr(w, http.StatusBadRequest, errors.New("path required"))
		return
	}
	actor := req.Actor
	if actor == "" {
		actor = s.actor
	}
	content := req.Content
	err = s.daemon.SafeWrite(r.Context(), projectID, req.Path,
		func([]byte) []byte { return content }, actor, req.SessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	projectID, err := s.authorize(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	paths, err := s.daemon.List(r.Context(), projectID, r.URL.Query().Get("prefix"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if paths == nil {
		paths = []string{}
	}
	writeJSON(w, http.StatusOK, listResponse{Paths: paths})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	projectID, err := s.authorize(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		writeErr(w, http.StatusBadRequest, errors.New("q required"))
		return
	}
	hits, err := s.daemon.Search(r.Context(), projectID, r.URL.Query().Get("prefix"), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if hits == nil {
		hits = []SearchHit{}
	}
	writeJSON(w, http.StatusOK, searchResponse{Results: hits})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr returns a JSON error. Internal errors are logged upstream; the body
// carries the message so the SDK can surface something actionable.
func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
