package admin

import (
	"net/http"
	"strings"
)

// versionRow is one version of a memory path, ready to render.
type versionRow struct {
	VersionID string
	ETag      string
	// IsHead marks the current content. The head cannot be redacted — removing
	// it would silently revert the path to older content — so the view disables
	// the control rather than letting the operator discover that on submit.
	IsHead bool
}

// redactionRow is one recorded redaction.
type redactionRow struct {
	VersionID  string
	Reason     string
	RedactedAt string
	RedactedBy string
}

// handleRedactView renders one path's version history with the redaction
// control, plus what has already been redacted.
//
// Lives in the console rather than the dashboard on purpose: the dashboard is
// read-only and runs as the restricted silo-runtime identity, and redaction
// destroys data irreversibly. Putting it there would hand every dashboard
// viewer a delete button.
func (s *Server) handleRedactView(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.FormValue("project"))
	path := strings.TrimSpace(r.FormValue("path"))
	if projectID == "" {
		http.Redirect(w, r, "/projects", http.StatusSeeOther)
		return
	}

	data := map[string]any{
		"Active":    "projects",
		"Project":   projectID,
		"Path":      path,
		"CanRedact": s.redact != nil,
	}

	if s.memory == nil {
		data["VersionsErr"] = "no backend configured, so version history is unavailable"
		s.render(w, "redact.html", data)
		return
	}

	// No path chosen yet — offer the project's objects to pick from.
	if path == "" {
		if paths, err := s.memory.ListPaths(r.Context(), projectID, ""); err != nil {
			data["VersionsErr"] = err.Error()
		} else {
			data["Paths"] = paths
		}
		s.render(w, "redact.html", data)
		return
	}

	versions, err := s.memory.ListVersions(r.Context(), projectID, path)
	if err != nil {
		data["VersionsErr"] = err.Error()
		s.render(w, "redact.html", data)
		return
	}
	rows := make([]versionRow, 0, len(versions))
	for i, v := range versions {
		// ListVersions is newest-first, so index 0 is the head.
		rows = append(rows, versionRow{VersionID: v.VersionID, ETag: v.ETag, IsHead: i == 0})
	}
	data["Versions"] = rows

	if s.redact != nil {
		if reds, err := s.redact.ListRedactions(r.Context(), projectID, path); err != nil {
			data["RedactionsErr"] = err.Error()
		} else {
			out := make([]redactionRow, 0, len(reds))
			for _, red := range reds {
				out = append(out, redactionRow{
					VersionID:  red.VersionID,
					Reason:     red.Reason,
					RedactedAt: red.RedactedAt,
					RedactedBy: red.RedactedBy,
				})
			}
			data["Redactions"] = out
		}
	}

	s.render(w, "redact.html", data)
}

// handleRedact destroys one version's content.
//
// Requires a typed confirmation of the version ID, matching the
// typed-project-ID gate on delete-bucket. Redaction is irreversible and there is
// no undo, so a reflexive click must not be able to destroy a version — and
// unlike teardown, the operator is choosing among near-identical opaque IDs
// where a misclick is easy.
func (s *Server) handleRedact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	projectID := strings.TrimSpace(r.FormValue("project"))
	path := strings.TrimSpace(r.FormValue("path"))
	versionID := strings.TrimSpace(r.FormValue("version"))
	confirm := strings.TrimSpace(r.FormValue("confirm"))
	reason := strings.TrimSpace(r.FormValue("reason"))

	back := "/redact?project=" + urlEscape(projectID) + "&path=" + urlEscape(path)

	if projectID == "" || path == "" || versionID == "" {
		redirectErr(w, r, back, "project, path, and version are all required")
		return
	}
	if s.redact == nil {
		redirectErr(w, r, back, "redaction is not configured on this console")
		return
	}
	if confirm != versionID {
		redirectErr(w, r, back,
			"type the version ID exactly to confirm — redaction destroys the content permanently")
		return
	}
	// The reason is what an operator reading this in six months actually needs.
	// Required here rather than in the schema, so the audit row is never a bare
	// timestamp.
	if reason == "" {
		redirectErr(w, r, back, "a reason is required: it is the only record of why the content was destroyed")
		return
	}

	if err := s.redact.RedactVersion(r.Context(), projectID, path, versionID, reason, actorFrom(r)); err != nil {
		// Surfaced verbatim. The daemon distinguishes a refusal from "the content
		// was destroyed but the audit row failed", and the second message tells
		// the operator to record it by hand — paraphrasing it here would lose
		// exactly the part that matters.
		redirectErr(w, r, back, err.Error())
		return
	}
	http.Redirect(w, r, back+"&redacted=1", http.StatusSeeOther)
}
