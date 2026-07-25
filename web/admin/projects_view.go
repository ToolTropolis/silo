package admin

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/tooltropolis/silo/internal/project"
	"github.com/tooltropolis/silo/internal/registry"
)

// projectRow is one project's lifecycle state.
type projectRow struct {
	Record registry.ProjectRecord
	// NextStep is the teardown layer this project is due for, empty when it is
	// not being torn down.
	NextStep string
	// Irreversible marks the step that destroys data, so the UI can demand a
	// stronger confirmation for it.
	Irreversible bool
	// StepLabel and StepDetail describe the next step in the operator's terms.
	// The raw step name ("revoke-credential") names an internal layer, which
	// reads like a key rotation to someone who came here to delete a project.
	StepLabel  string
	StepDetail string
	// StepNumber is this step's position in the four-step sequence.
	StepNumber int
	// Pending is unsynced writes still on a daemon's disk. Tearing down a
	// project that has them loses those writes, so the view surfaces it next to
	// the teardown control rather than only on the cache page.
	Pending int
	// PendingKnown is false when no daemon could be reached — the view then
	// says so rather than showing a reassuring zero.
	PendingKnown bool
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		s.fail(w, "projects", errors.New("no registry configured"))
		return
	}
	ctx := r.Context()

	recs, err := s.registry.List(ctx)
	if err != nil {
		s.fail(w, "projects", err)
		return
	}

	pending := map[string]int{}
	daemonUp := false
	if s.daemon != nil {
		if stats, err := s.daemon.CacheStats(ctx); err == nil {
			daemonUp = true
			for _, st := range stats {
				pending[st.Project] = st.Pending
			}
		}
	}

	rows := make([]projectRow, 0, len(recs))
	for _, rec := range recs {
		row := projectRow{Record: rec, PendingKnown: daemonUp, Pending: pending[rec.ProjectID]}
		if s.prov != nil {
			if steps, err := s.prov.TeardownPlan(ctx, rec.ProjectID); err == nil {
				for _, st := range steps {
					if !st.Done {
						row.NextStep = st.Name
						row.Irreversible = st.Name == "delete-bucket"
						row.StepLabel, row.StepDetail, row.StepNumber = describeStep(st.Name)
						break
					}
				}
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Record.ProjectID < rows[j].Record.ProjectID })

	s.render(w, "projects.html", map[string]any{
		"Active":   "projects",
		"Subtitle": "Onboard, inspect, and tear down tenants",
		"Rows":     rows,
		"CanProv":  s.prov != nil,
		"DaemonUp": daemonUp,
		"Flash":    r.URL.Query().Get("flash"),
		"FlashErr": r.URL.Query().Get("err"),
	})
}

func (s *Server) handleOnboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.prov == nil {
		redirectErr(w, r, "/projects", "no provisioner configured")
		return
	}
	projectID := strings.TrimSpace(r.FormValue("project"))

	// Validate before calling: onboarding bakes the ID into a bucket name and a
	// cache filename permanently, so a clear message here beats a partially
	// provisioned project.
	if err := project.ValidateID(projectID); err != nil {
		redirectErr(w, r, "/projects", err.Error())
		return
	}
	if err := s.prov.Onboard(r.Context(), projectID, "", ""); err != nil {
		redirectErr(w, r, "/projects", err.Error())
		return
	}
	redirectFlash(w, r, "/projects", projectID+": onboarded")
}

// handleTeardown runs ONE teardown layer.
//
// Per-layer and individually confirmed, exactly as in siloctl — this is not a
// one-click destroy. The irreversible step additionally requires the operator
// to type the project ID, so a misplaced click cannot delete a bucket.
func (s *Server) handleTeardown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.prov == nil {
		redirectErr(w, r, "/projects", "no provisioner configured")
		return
	}
	projectID := r.FormValue("project")
	step := r.FormValue("step")
	if projectID == "" || step == "" {
		redirectErr(w, r, "/projects", "project and step required")
		return
	}

	// The typed-ID confirmation for the destructive step, mirroring siloctl's.
	if step == "delete-bucket" {
		if strings.TrimSpace(r.FormValue("confirm")) != projectID {
			redirectErr(w, r, "/projects",
				"delete-bucket is irreversible: type the project ID exactly to confirm")
			return
		}
	}

	msg, err := s.prov.TeardownStep(r.Context(), projectID, step)
	if err != nil {
		redirectErr(w, r, "/projects", err.Error())
		return
	}
	if msg == "" {
		msg = projectID + ": " + step + " complete"
	}
	redirectFlash(w, r, "/projects", msg)
}

// describeStep renders a teardown step in the operator's terms.
//
// The step names are the internal layer names, and they mislead at exactly the
// wrong moment: someone deleting a project sees "revoke-credential", which
// sounds like rotating a key rather than the first step of destroying
// everything. Each label says what it does; the detail says what it destroys.
func describeStep(step string) (label, detail string, number int) {
	switch step {
	case "revoke-credential":
		return "Start deleting", "Step 1 of 4 — revokes agent tokens and the S3 credential. " +
			"Agents lose access immediately.", 1
	case "revoke-key":
		return "Continue deleting", "Step 2 of 4 — destroys the per-project encryption key.", 2
	case "delete-bucket":
		return "Delete all memory", "Step 3 of 4 — destroys the bucket, every stored version, " +
			"and the local cache. This cannot be undone.", 3
	case "deregister":
		return "Finish deleting", "Step 4 of 4 — removes the registry record and cache policy.", 4
	}
	return step, "", 0
}
