package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
)

// Grant is the access a verified token authorizes: one project, at one scope.
//
// The zero value grants nothing, since an empty ProjectID is not a valid
// project. That matters because it makes the failure mode of a dropped error
// a denial rather than an authorization against "".
type Grant struct {
	ProjectID string
	// ReadOnly means writes must be refused for this token.
	ReadOnly bool
}

// TokenVerifier maps a bearer token presented by an agent to the access it
// grants. It's the daemon's authorization boundary: a token resolves to exactly
// one project, so an agent can never address another project's memory
// regardless of what it puts in the request.
type TokenVerifier interface {
	// ProjectFor returns the access a token grants, or ErrUnauthorized.
	//
	// Returns scope alongside the project rather than exposing a second
	// ScopeFor method: a separate lookup is one a handler can forget, and
	// forgetting it would silently grant write access to a read-only token.
	// Carrying both means a handler that ignores the scope is a visible
	// omission at the call site.
	ProjectFor(token string) (Grant, error)
}

// ErrUnauthorized is returned by a TokenVerifier for an unknown token.
var ErrUnauthorized = errors.New("daemon: unauthorized")

// ErrReadOnlyToken is returned when a write is attempted with a read-only
// token. Distinct from ErrUnauthorized because the token IS valid — the
// operation is not permitted — and an agent that cannot tell those apart will
// retry a write that can never succeed.
var ErrReadOnlyToken = errors.New("daemon: token is read-only")

// StaticTokenVerifier is a simple in-memory token->project map, suitable for
// local/dev use and tests. Production wiring resolves tokens against the
// credential store issued at onboarding.
//
// Every static token is read-write. --tokens is a dev convenience with no way
// to express a scope, and inventing a syntax for it would put an authorization
// decision in a flag string; mint a read-only token from the registry instead.
type StaticTokenVerifier map[string]string

// ProjectFor implements TokenVerifier.
func (s StaticTokenVerifier) ProjectFor(token string) (Grant, error) {
	if p, ok := s[token]; ok && token != "" {
		return Grant{ProjectID: p}, nil
	}
	return Grant{}, ErrUnauthorized
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
	mux.HandleFunc("/v1/queue", s.handleQueue)
	mux.HandleFunc("/v1/sync", s.handleSync)
	return mux
}

// syncResponse reports what a forced drain achieved.
type syncResponse struct {
	Project   string `json:"project"`
	Drained   int    `json:"drained"`
	Remaining int    `json:"remaining"`
	Error     string `json:"error,omitempty"`
}

// handleSync drains the caller's queue now instead of waiting for the next tick.
//
// The sync worker gets there eventually, but "eventually" is not good enough
// before a shutdown or a teardown, where the question is whether it is safe to
// destroy something. Scoped to the token's project like every other route.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	grant, err := s.authorize(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return
	}

	before, err := s.daemon.QueueDepth(r.Context(), grant.ProjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	syncErr := s.daemon.SyncProject(r.Context(), grant.ProjectID)

	after, err := s.daemon.QueueDepth(r.Context(), grant.ProjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	resp := syncResponse{Project: grant.ProjectID, Drained: before - after, Remaining: after}
	if syncErr != nil {
		// Still 200: the counts are the answer, and a caller deciding whether
		// it is safe to tear down needs the numbers more than a status code.
		// Remaining > 0 is the signal that it is not.
		resp.Error = syncErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// queueResponse reports how much of a project's memory is still only on this
// host's disk.
type queueResponse struct {
	Project        string `json:"project"`
	Pending        int    `json:"pending"`
	OldestQueuedAt string `json:"oldest_queued_at,omitempty"`
}

// handleQueue answers "is any of my memory still unsynced?".
//
// It takes no project parameter: the project comes from the bearer token like
// every other route, so an agent can ask about its own silo and nothing else.
// Fleet-wide queue state is an operator question and deliberately does not live
// behind an agent's token.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	grant, err := s.authorize(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	depth, err := s.daemon.QueueDepth(r.Context(), grant.ProjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := queueResponse{Project: grant.ProjectID, Pending: depth}
	if depth > 0 {
		// Best-effort: the depth is the answer, and an unreadable timestamp
		// shouldn't fail the request.
		if oldest, err := s.daemon.OldestQueued(r.Context(), grant.ProjectID); err == nil {
			resp.OldestQueuedAt = oldest
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// Listen binds addr without serving. A path (no colon) is treated as a Unix
// socket; anything else as a TCP address.
//
// Separate from Serve so a caller can report "listening on X" only after the
// bind actually succeeded. Printing before the bind makes a port conflict look
// like a successful start, and the next symptom — every request failing
// against whatever process already owns the port — is baffling.
func (s *Server) Listen(addr string) (net.Listener, error) { return Listen(addr) }

// Listen binds addr, treating a colon-free value as a Unix socket path.
//
// Shared by the agent-facing and admin listeners so the socket-vs-TCP rule is
// stated once. A stale socket file is removed first: bind fails with "address
// already in use" otherwise, which after an unclean shutdown looks exactly like
// a port conflict and sends you looking in the wrong place.
func Listen(addr string) (net.Listener, error) {
	network := "tcp"
	if !strings.Contains(addr, ":") {
		network = "unix"
		if err := removeStaleSocket(addr); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen %s %s: %w", network, addr, err)
	}
	if network == "unix" {
		// The socket is the authorization boundary for the admin surface, so it
		// must not be world-accessible.
		if err := os.Chmod(addr, 0o700); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("daemon: secure socket %s: %w", addr, err)
		}
	}
	return ln, nil
}

// removeStaleSocket clears a socket file left behind by an unclean exit, but
// only if nothing is listening on it — removing a live one would silently
// detach a running daemon from its listener.
func removeStaleSocket(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("daemon: stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("daemon: %s exists and is not a socket", path)
	}
	if conn, err := net.Dial("unix", path); err == nil {
		_ = conn.Close()
		return fmt.Errorf("daemon: %s is already in use by a running process", path)
	}
	return os.Remove(path)
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

// authorize resolves the request's bearer token to the access it grants.
//
// Returns a Grant rather than a projectID so a handler holds the scope it needs
// to authorize the operation, with no second lookup to forget. Read handlers
// use only grant.ProjectID; write handlers must also call authorizeWrite.
func (s *Server) authorize(r *http.Request) (Grant, error) {
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == auth || token == "" { // missing or malformed
		return Grant{}, ErrUnauthorized
	}
	return s.verifier.ProjectFor(token)
}

// authorizeWrite resolves the token and additionally refuses a read-only one.
//
// Separate from authorize rather than a boolean parameter so that the write
// path reads as a distinct decision at the call site: a handler that mutates
// memory calls a differently-named function, and one that forgets is visible in
// review rather than hidden behind an argument.
func (s *Server) authorizeWrite(r *http.Request) (Grant, error) {
	grant, err := s.authorize(r)
	if err != nil {
		return Grant{}, err
	}
	if grant.ReadOnly {
		return Grant{}, ErrReadOnlyToken
	}
	return grant, nil
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

// writeResponse tells the caller whether the write is actually safe.
//
// Durable is the field that matters: false means the content is buffered on the
// daemon's local disk and has not reached the versioned backend yet. Callers
// must NOT retry a queued write — it is already enqueued, and retrying just
// buffers a duplicate.
type writeResponse struct {
	Status  string `json:"status"`
	Durable bool   `json:"durable"`
}

type listResponse struct {
	Paths []string `json:"paths"`
}

type searchResponse struct {
	Results []SearchHit `json:"results"`
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	grant, err := s.authorize(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, errors.New("path required"))
		return
	}
	content, err := s.daemon.Read(r.Context(), grant.ProjectID, path)
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
	// authorizeWrite, not authorize: this is the only route that puts new
	// content into a project's memory, so it is the one a read-only token must
	// not reach.
	grant, err := s.authorizeWrite(r)
	if err != nil {
		if errors.Is(err, ErrReadOnlyToken) {
			// 403, not 401: the token authenticated fine. A 401 invites the
			// client to re-authenticate, which cannot help — no retry with this
			// credential will ever succeed.
			writeErr(w, http.StatusForbidden, err)
			return
		}
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
	outcome, err := s.daemon.SafeWrite(r.Context(), grant.ProjectID, req.Path,
		func([]byte) []byte { return content }, actor, req.SessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// 202 for a write that only reached local disk. It is a success — the write
	// was accepted and will be replayed — but a caller that cannot tell the
	// difference will report data as safe when it is one disk failure from gone.
	if outcome == WriteQueued {
		writeJSON(w, http.StatusAccepted, writeResponse{
			Status:  "queued",
			Durable: false,
		})
		return
	}
	writeJSON(w, http.StatusOK, writeResponse{Status: "ok", Durable: true})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	grant, err := s.authorize(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	paths, err := s.daemon.List(r.Context(), grant.ProjectID, r.URL.Query().Get("prefix"))
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
	grant, err := s.authorize(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		writeErr(w, http.StatusBadRequest, errors.New("q required"))
		return
	}
	hits, err := s.daemon.Search(r.Context(), grant.ProjectID, r.URL.Query().Get("prefix"), q)
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
