package daemon

import (
	"errors"
	"net/http"
)

// AdminServer is the daemon's operator surface: fleet-wide and destructive
// operations that must not sit behind an agent's bearer token.
//
// Kept separate from Server for the reason handleQueue documents — an agent
// token answers for exactly one project, and widening it to cover operator
// actions would put "delete this project's local state" behind a credential
// handed to an automated caller.
//
// Authorization is the listener itself. Bound to a Unix socket, the filesystem
// permissions are the check: if you can open the socket, you are the operator.
type AdminServer struct {
	daemon   *Daemon
	projects func() []string
}

// NewAdminServer builds the operator surface. projects supplies the set this
// daemon manages, so the fleet view does not need a token per project.
func NewAdminServer(d *Daemon, projects func() []string) *AdminServer {
	if projects == nil {
		projects = func() []string { return nil }
	}
	return &AdminServer{daemon: d, projects: projects}
}

// Handler returns the routed admin handler.
func (a *AdminServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/admin/purge-cache", a.handlePurgeCache)
	mux.HandleFunc("/v1/admin/cache-stats", a.handleCacheStats)
	mux.HandleFunc("/v1/admin/compact-cache", a.handleCompactCache)
	return mux
}

type purgeResponse struct {
	Project string `json:"project"`
	Purged  bool   `json:"purged"`
	Pending int    `json:"pending,omitempty"`
	Error   string `json:"error,omitempty"`
}

// handlePurgeCache drops a project's local cache, refusing while it holds
// unsynced writes.
func (a *AdminServer) handlePurgeCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return
	}
	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("project required"))
		return
	}

	err := a.daemon.PurgeCache(r.Context(), projectID)
	if errors.Is(err, ErrQueuedWrites) {
		// 409: the request is well-formed but conflicts with state the caller
		// needs to resolve first. The depth tells them how much.
		depth, _ := a.daemon.QueueDepth(r.Context(), projectID)
		writeJSON(w, http.StatusConflict, purgeResponse{
			Project: projectID,
			Pending: depth,
			Error:   err.Error(),
		})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, purgeResponse{Project: projectID, Purged: true})
}

type compactResponse struct {
	Project     string `json:"project"`
	Compacted   bool   `json:"compacted"`
	Reclaimed   int64  `json:"reclaimed_bytes"`
	BytesBefore int64  `json:"bytes_before"`
	BytesAfter  int64  `json:"bytes_after"`
	SkipReason  string `json:"skip_reason,omitempty"`
}

// handleCompactCache rewrites a project's cache file, returning the disk that
// eviction freed but bbolt kept.
//
// A skip is 200, not an error: refusing while writes are queued is the designed
// safe behaviour, and the caller is told why rather than being handed a failure
// to interpret.
func (a *AdminServer) handleCompactCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return
	}
	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("project required"))
		return
	}

	res, err := a.daemon.CompactCache(r.Context(), projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, compactResponse{
		Project:     projectID,
		Compacted:   !res.Skipped,
		Reclaimed:   res.Reclaimed(),
		BytesBefore: res.BytesBefore,
		BytesAfter:  res.BytesAfter,
		SkipReason:  res.SkipReason,
	})
}

type cacheStat struct {
	Project string `json:"project"`
	Pending int    `json:"pending"`
	Oldest  string `json:"oldest_queued_at,omitempty"`
	// Entries and Bytes describe live cached content; FileBytes is what the
	// file actually occupies. The console shows the gap between the last two,
	// since that is precisely what compaction reclaims.
	Entries   int   `json:"entries"`
	Bytes     int64 `json:"bytes"`
	FileBytes int64 `json:"file_bytes"`
	// StatsError records why the size fields are absent, so the console can
	// show "unknown" rather than rendering a fabricated zero.
	StatsError string `json:"stats_error,omitempty"`
}

// handleCacheStats reports local cache state across every project this daemon
// manages — the operator view that /v1/queue deliberately cannot provide.
func (a *AdminServer) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	stats := []cacheStat{}
	for _, projectID := range a.projects() {
		depth, err := a.daemon.QueueDepth(r.Context(), projectID)
		if err != nil {
			continue // a project this daemon cannot read is not this view's problem
		}
		s := cacheStat{Project: projectID, Pending: depth}
		if depth > 0 {
			if oldest, err := a.daemon.OldestQueued(r.Context(), projectID); err == nil {
				s.Oldest = oldest
			}
		}
		if cs, err := a.daemon.CacheStats(r.Context(), projectID); err == nil {
			s.Entries, s.Bytes, s.FileBytes = cs.Entries, cs.Bytes, cs.FileBytes
		} else {
			s.StatsError = err.Error()
		}
		stats = append(stats, s)
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": stats})
}
