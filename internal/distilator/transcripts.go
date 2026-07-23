package distilator

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// SessionPrefix is where captured session transcripts live in a project's
// store, separate from both the live memory store and the run output.
const SessionPrefix = "_sessions"

// StoreTranscripts reads session transcripts a project has captured into its
// own store under SessionPrefix. Because it reads through the same
// project-scoped Store, a run can only ever see its own project's sessions.
//
// sinceHours is not yet applied as a filter: transcripts are returned in path
// order (which is chronological when session IDs are time-ordered) and bounded
// by Limit. Time-based filtering needs a capture timestamp, which lands with
// internal/transcript.
type StoreTranscripts struct {
	store Store
	// Limit caps how many transcripts one run consolidates. Zero means
	// DefaultTranscriptLimit.
	Limit int
}

// DefaultTranscriptLimit bounds a batch so one run can't pull an unbounded
// amount of session content into a single request.
const DefaultTranscriptLimit = 50

// NewStoreTranscripts reads transcripts from a project's store.
func NewStoreTranscripts(s Store) *StoreTranscripts { return &StoreTranscripts{store: s} }

var _ TranscriptSource = (*StoreTranscripts)(nil)

// Recent returns up to Limit captured transcripts for a project.
func (s *StoreTranscripts) Recent(ctx context.Context, projectID string, sinceHours int) ([]Transcript, error) {
	_ = sinceHours // see the type comment: needs a capture timestamp to honor

	paths, err := s.store.List(ctx, projectID, SessionPrefix+"/")
	if err != nil {
		return nil, fmt.Errorf("distilator: list sessions for %q: %w", projectID, err)
	}

	limit := s.Limit
	if limit <= 0 {
		limit = DefaultTranscriptLimit
	}

	var out []Transcript
	for _, p := range paths {
		if len(out) >= limit {
			break
		}
		content, err := s.store.Read(ctx, projectID, p)
		if err != nil {
			return nil, fmt.Errorf("distilator: read session %q: %w", p, err)
		}
		out = append(out, Transcript{SessionID: sessionIDFromPath(p), Messages: content})
	}
	return out, nil
}

// sessionIDFromPath recovers the session ID from its object path, trimming the
// prefix and any file extension.
func sessionIDFromPath(p string) string {
	base := path.Base(p)
	return strings.TrimSuffix(base, path.Ext(base))
}

// SessionPath is where a session's transcript is stored for a project.
func SessionPath(sessionID string) string {
	return path.Join(SessionPrefix, sessionID+".json")
}
