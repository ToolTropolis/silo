package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tooltropolis/silo/internal/project"
)

// wizardSteps names the onboarding flow in order. Each is a real boundary, not
// a decorative page: naming is permanent, preflight is what turns most
// provisioning failures into a message shown before anything is created, and
// review states exactly what will exist afterwards.
var wizardSteps = []struct {
	Key   string
	Label string
	Blurb string
}{
	{"name", "Name", "Choose the project ID"},
	{"checks", "Checks", "Verify onboarding will succeed"},
	{"review", "Review", "Confirm what will be created"},
	{"connect", "Connect", "Wire a repo to this project"},
	{"done", "Done", "Start using it"},
}

// ProvisionState tracks one in-flight or finished onboarding.
//
// Held in memory rather than persisted: it describes a single operator's
// several-second flow, and the registry is already the durable record of what
// exists. A restart mid-provision loses the progress display, not the project.
type ProvisionState struct {
	ProjectID string
	Started   time.Time
	Done      bool
	Err       string
	// Layers mirrors the Onboarder's four steps so the UI can show which one
	// failed rather than only that something did.
	Layers []LayerState
}

// LayerState is one provisioning layer's progress.
type LayerState struct {
	Name   string
	Status CheckStatus
	Detail string
}

// provisionTracker holds ProvisionStates by project ID.
type provisionTracker struct {
	mu     sync.Mutex
	states map[string]*ProvisionState
}

func newProvisionTracker() *provisionTracker {
	return &provisionTracker{states: map[string]*ProvisionState{}}
}

func (t *provisionTracker) start(projectID string) *ProvisionState {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := &ProvisionState{
		ProjectID: projectID,
		Started:   time.Now(),
		Layers: []LayerState{
			{Name: "Registry record", Status: CheckWarn, Detail: "pending"},
			{Name: "Per-project encryption key", Status: CheckWarn, Detail: "pending"},
			{Name: "Versioned bucket", Status: CheckWarn, Detail: "pending"},
			{Name: "Scoped S3 credential", Status: CheckWarn, Detail: "pending"},
		},
	}
	t.states[projectID] = st
	return st
}

func (t *provisionTracker) get(projectID string) (*ProvisionState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.states[projectID]
	if !ok {
		return nil, false
	}
	// Return a copy: the caller renders it while the provisioning goroutine may
	// still be writing.
	clone := *st
	clone.Layers = append([]LayerState(nil), st.Layers...)
	return &clone, true
}

func (t *provisionTracker) finish(projectID string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.states[projectID]
	if !ok {
		return
	}
	st.Done = true
	if err != nil {
		st.Err = err.Error()
		// Mark the layer that failed. The Onboarder wraps its errors with the
		// step name, so match on that rather than guessing from position.
		marked := false
		for i := range st.Layers {
			if marked {
				st.Layers[i].Status = CheckWarn
				st.Layers[i].Detail = "not attempted"
				continue
			}
			if layerMatchesError(st.Layers[i].Name, err.Error()) {
				st.Layers[i].Status = CheckFail
				st.Layers[i].Detail = "failed — rolled back"
				marked = true
				continue
			}
			st.Layers[i].Status = CheckWarn
			st.Layers[i].Detail = "rolled back"
		}
		return
	}
	for i := range st.Layers {
		st.Layers[i].Status = CheckPass
		st.Layers[i].Detail = "created"
	}
}

// layerMatchesError maps an Onboarder error onto the layer that produced it.
func layerMatchesError(layer, errMsg string) bool {
	m := strings.ToLower(errMsg)
	switch layer {
	case "Registry record":
		return strings.Contains(m, "register:")
	case "Per-project encryption key":
		return strings.Contains(m, "create key")
	case "Versioned bucket":
		return strings.Contains(m, "create bucket")
	case "Scoped S3 credential":
		return strings.Contains(m, "issue credential")
	}
	return false
}

// handleWizard serves the onboarding flow. One URL per step, so a step is
// linkable, refreshable, and back-button-safe — none of which a single form
// with hidden state gives you.
func (s *Server) handleWizard(w http.ResponseWriter, r *http.Request) {
	step := strings.TrimPrefix(r.URL.Path, "/onboard/")
	if step == "" || step == "/" {
		http.Redirect(w, r, "/onboard/name", http.StatusSeeOther)
		return
	}

	switch step {
	case "name":
		s.wizardName(w, r)
	case "checks":
		s.wizardChecks(w, r)
	case "review":
		s.wizardReview(w, r)
	case "provision":
		s.wizardProvision(w, r)
	case "status":
		s.wizardStatus(w, r)
	case "connect":
		s.wizardConnect(w, r)
	case "mint":
		s.wizardMint(w, r)
	case "write":
		s.wizardWrite(w, r)
	case "done":
		s.wizardDone(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) wizardName(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project"))
	data := s.wizardData("name", projectID)

	// Validate only once something has been typed, so the form does not open
	// with an error on it.
	if projectID != "" {
		if err := project.ValidateID(projectID); err != nil {
			data["IDError"] = err.Error()
		} else {
			data["Bucket"] = "silo-" + projectID
			data["CacheFile"] = projectID + ".bbolt"
		}
	}
	s.render(w, "wizard_name.html", data)
}

func (s *Server) wizardChecks(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.FormValue("project"))
	if projectID == "" {
		http.Redirect(w, r, "/onboard/name", http.StatusSeeOther)
		return
	}

	pf := &Preflighter{
		Registry: s.registry,
		Daemon:   s.daemon,
		Backend:  s.backendProbe,
		Creds:    s.credsProbe,
	}
	report := pf.Run(r.Context(), projectID)

	data := s.wizardData("checks", projectID)
	data["Report"] = report
	s.render(w, "wizard_checks.html", data)
}

func (s *Server) wizardReview(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.FormValue("project"))
	if projectID == "" {
		http.Redirect(w, r, "/onboard/name", http.StatusSeeOther)
		return
	}
	if err := project.ValidateID(projectID); err != nil {
		http.Redirect(w, r, "/onboard/name?project="+urlEscape(projectID), http.StatusSeeOther)
		return
	}

	data := s.wizardData("review", projectID)
	data["Bucket"] = "silo-" + projectID
	data["CacheFile"] = projectID + ".bbolt"
	data["KeyID"] = "projects/" + projectID
	data["CredentialID"] = "silo-cred-" + projectID
	s.render(w, "wizard_review.html", data)
}

// wizardProvision starts onboarding and redirects to the progress view.
//
// Runs in the background so the request returns immediately: issuing a
// credential can take seconds, and a browser sitting on a POST with no feedback
// is exactly the experience the wizard exists to replace.
func (s *Server) wizardProvision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.prov == nil {
		redirectErr(w, r, "/onboard/name", "no provisioner configured")
		return
	}
	projectID := strings.TrimSpace(r.FormValue("project"))
	if err := project.ValidateID(projectID); err != nil {
		redirectErr(w, r, "/onboard/name", err.Error())
		return
	}

	s.tracker.start(projectID)
	go func() {
		// Detached from the request: the operator's browser may navigate away,
		// and provisioning must not be cancelled by that. Bounded so a hung
		// credential issuer cannot leak a goroutine forever.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
		defer cancel()
		s.tracker.finish(projectID, s.prov.Onboard(ctx, projectID))
	}()

	http.Redirect(w, r, "/onboard/status?project="+urlEscape(projectID), http.StatusSeeOther)
}

// wizardStatus renders provisioning progress, refreshing until it settles.
func (s *Server) wizardStatus(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.FormValue("project"))
	state, ok := s.tracker.get(projectID)
	if !ok {
		http.Redirect(w, r, "/onboard/name", http.StatusSeeOther)
		return
	}

	data := s.wizardData("review", projectID)
	data["State"] = state
	s.render(w, "wizard_status.html", data)
}

func (s *Server) wizardDone(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.FormValue("project"))
	if projectID == "" {
		http.Redirect(w, r, "/onboard/name", http.StatusSeeOther)
		return
	}
	data := s.wizardData("done", projectID)
	data["Bucket"] = "silo-" + projectID
	s.render(w, "wizard_done.html", data)
}

// wizardData builds the shared template payload, including the step rail.
func (s *Server) wizardData(step, projectID string) map[string]any {
	type railStep struct {
		Key, Label, Blurb string
		State             string // "done" | "current" | "todo"
	}
	current := 0
	for i, st := range wizardSteps {
		if st.Key == step {
			current = i
		}
	}
	rail := make([]railStep, 0, len(wizardSteps))
	for i, st := range wizardSteps {
		state := "todo"
		switch {
		case i < current:
			state = "done"
		case i == current:
			state = "current"
		}
		rail = append(rail, railStep{Key: st.Key, Label: st.Label, Blurb: st.Blurb, State: state})
	}
	return map[string]any{
		"Active":  "projects",
		"Step":    step,
		"Rail":    rail,
		"Project": projectID,
	}
}

// errNoProvisioner is returned when provisioning is attempted without one.
var errNoProvisioner = errors.New("no provisioner configured")

// probeFunc adapts a plain function into a Backend/Credential prober.
type probeFunc func(context.Context) error

func (f probeFunc) Probe(ctx context.Context) error { return f(ctx) }

// describeDuration renders how long provisioning has been running.
func describeDuration(d time.Duration) string {
	if d < time.Second {
		return "just now"
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
