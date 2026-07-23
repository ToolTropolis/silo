package dashboard

import (
	"errors"
	"net/http"

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
		rows = append(rows, projectRow{
			ProjectID:    rec.ProjectID,
			Status:       rec.Status,
			BucketName:   rec.BucketName,
			CredentialID: rec.CredentialID,
			KeyID:        rec.KeyID,
			CreatedAt:    rec.CreatedAt,
			NextStep:     teardownHint(rec.Status),
		})
	}
	s.render(w, "registry.html", map[string]any{"Projects": rows, "Active": "registry"})
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
