package dashboard

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/tooltropolis/silo/internal/registry"
)

// projectRow is one row of the registry table. It carries references only —
// CredentialID and KeyID are opaque pointers into the secrets store and KMS,
// never the credential or key material itself.
type projectRow struct {
	ProjectID    string
	Status       string
	BucketName   string
	CredentialID string
	KeyID        string
	CreatedAt    string
	// NextStep tells an operator which teardown command to run when a project
	// is stuck mid-decommission (spec §7.1). The dashboard never runs it.
	NextStep string
	// Unsynced counts writes still buffered on the daemon's disk. Rendered as
	// "?" when no QueueReader is configured, because "nothing pending" and
	// "nobody checked" must never look the same.
	Unsynced string
	// UnsyncedClass emphasises a non-zero count. Data at risk is a warning, not
	// a statistic, and deciding that here keeps the template readable.
	UnsyncedClass string
}

// handleRegistry is the dashboard home page: a read-only table of every project.
//
// This view never issues a credential, revokes a key, or deletes a bucket —
// those stay exclusively in siloctl teardown's confirmed per-layer CLI flow.
func (s *Server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		s.fail(w, "Registry", errors.New("no registry configured (pass --rqlite)"))
		return
	}
	records, err := s.registry.List(r.Context())
	if err != nil {
		s.fail(w, "Registry", err)
		return
	}

	rows := make([]projectRow, 0, len(records))
	for _, rec := range records {
		unsynced := s.unsyncedFor(r.Context(), rec.ProjectID)
		rows = append(rows, projectRow{
			Unsynced:      unsynced,
			UnsyncedClass: unsyncedClass(unsynced),
			ProjectID:     rec.ProjectID,
			Status:        rec.Status,
			BucketName:    rec.BucketName,
			CredentialID:  rec.CredentialID,
			KeyID:         rec.KeyID,
			CreatedAt:     rec.CreatedAt,
			NextStep:      teardownHint(rec.Status),
		})
	}
	s.render(w, "registry.html", map[string]any{"Projects": rows, "Active": "registry"})
}

// unsyncedFor reports a project's locally-buffered write count for display.
//
// Returns "?" when no QueueReader is wired or the lookup fails — a project that
// might be holding unsynced data must not render as a confident "0".
func (s *Server) unsyncedFor(ctx context.Context, projectID string) string {
	if s.queues == nil {
		return "?"
	}
	depth, err := s.queues.QueueDepth(ctx, projectID)
	if err != nil {
		return "?"
	}
	return strconv.Itoa(depth)
}

// unsyncedClass emphasises a project holding data that never reached the
// backend. "0" and "?" are unremarkable; anything else is a warning.
func unsyncedClass(unsynced string) string {
	if unsynced == "0" || unsynced == "?" {
		return "meta"
	}
	return "unsynced"
}

// teardownHint derives which manual teardown step to run next from a project's
// status. Decommissioning is the only state that needs operator action; the
// dashboard surfaces the command without ever executing it.
func teardownHint(status string) string {
	switch status {
	case registry.StatusDecommissioning:
		return "siloctl teardown --step=revoke-credential → revoke-key → delete-bucket → deregister"
	case registry.StatusDecommissioned:
		return "fully decommissioned"
	default:
		return ""
	}
}
