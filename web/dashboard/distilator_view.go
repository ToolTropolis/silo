package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/tooltropolis/silo/internal/distilator"
)

// isInternalPath reports whether a path is one of Silo's reserved namespaces —
// Distilator run output or captured session transcripts. Neither is "memory".
func isInternalPath(p string) bool {
	for _, prefix := range []string{distilator.OutputPrefix, distilator.SessionPrefix} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// proposalRow is one proposed change, rendered against the memory it would
// replace so a reviewer can see the actual diff before approving.
type proposalRow struct {
	Path       string
	Rationale  string
	Evidence   []string
	Prevalence float64
	Current    string // what's in the live store today ("" if the path is new)
	Proposed   string
	IsNew      bool
}

// handleRuns serves the Distilator review view (spec §7.3): list a project's
// runs, and show one run's proposals with their diffs for approval.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	if s.reviewer == nil {
		s.fail(w, "Distilations", errors.New("no reviewer configured (pass --backend-endpoint)"))
		return
	}
	ctx := r.Context()
	project := r.URL.Query().Get("project")
	runID := r.URL.Query().Get("run")

	data := map[string]any{
		"Active":  "distilations",
		"Project": project,
		"RunID":   runID,
		"Message": r.URL.Query().Get("msg"),
	}

	if project == "" {
		projects, err := s.projectIDs(r)
		if err != nil {
			s.fail(w, "Distilations", err)
			return
		}
		data["Projects"] = projects
		s.render(w, "distilations.html", data)
		return
	}

	runs, err := s.reviewer.ListRuns(ctx, project)
	if err != nil {
		s.fail(w, "Distilations", err)
		return
	}
	data["Runs"] = runs

	if runID == "" {
		s.render(w, "distilations.html", data)
		return
	}

	run, err := s.reviewer.LoadRun(ctx, project, runID)
	if err != nil {
		s.fail(w, "Distilations", err)
		return
	}

	rows := make([]proposalRow, 0, len(run.Proposals))
	for _, p := range run.Proposals {
		row := proposalRow{
			Path:       p.Path,
			Rationale:  p.Rationale,
			Evidence:   p.Evidence,
			Prevalence: p.Prevalence,
			Proposed:   string(p.NewContent),
		}
		// Show what this would replace. A read failure here means the path is
		// new, which is itself useful signal — not an error.
		if s.memory != nil {
			if current, _, err := s.memory.Get(ctx, project, p.Path, ""); err == nil {
				row.Current = string(current)
			} else {
				row.IsNew = true
			}
		}
		rows = append(rows, row)
	}
	data["Proposals"] = rows
	s.render(w, "distilations.html", data)
}

// handlePromote applies the proposals a human checked. This is the ONLY write
// action anywhere in the dashboard (spec §7.3); promotion routes through the
// daemon's SafeWrite, so an approved change gets the same CAS/versioning
// treatment as any other write.
//
// Rejection is simply not approving: unchecked proposals are never written, and
// the run's output stays in place for audit.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.reviewer == nil {
		s.fail(w, "Distilations", errors.New("no reviewer configured"))
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, "Distilations", err)
		return
	}
	project := r.FormValue("project")
	runID := r.FormValue("run")
	approved := r.Form["approve"]

	if project == "" || runID == "" {
		s.fail(w, "Distilations", errors.New("project and run are required"))
		return
	}

	var msg string
	if len(approved) == 0 {
		msg = "Nothing approved — no changes were written. The run's output is kept for audit."
	} else {
		promoted, err := s.reviewer.Promote(r.Context(), project, runID, approved)
		switch {
		case err != nil && len(promoted) > 0:
			// Partial promotion must be visible, not silently rolled up.
			msg = "Partially promoted " + strings.Join(promoted, ", ") + " — then failed: " + err.Error()
		case err != nil:
			s.fail(w, "Distilations", err)
			return
		default:
			msg = "Promoted " + strings.Join(promoted, ", ") + " through SafeWrite (tagged promoted_from:" + runID + ")."
		}
	}

	// Redirect back to the run so a refresh doesn't re-submit the approval.
	q := url.Values{"project": {project}, "run": {runID}, "msg": {msg}}
	http.Redirect(w, r, "/distilations?"+q.Encode(), http.StatusSeeOther)
}
