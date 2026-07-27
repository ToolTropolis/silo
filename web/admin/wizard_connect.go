package admin

import (
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// mintedToken is a freshly issued token held for exactly one page render.
//
// A token exists in plaintext only at creation, so the reveal has to happen
// once and then be gone. Held server-side and keyed by project rather than
// passed through a redirect, because a token in a URL lands in browser history,
// the referer header, and any proxy log in between — which would undo the point
// of never storing it.
type mintedToken struct {
	token   string
	label   string
	created time.Time
}

// tokenVault holds minted tokens until they are shown once.
type tokenVault struct {
	mu     sync.Mutex
	tokens map[string]mintedToken
}

func newTokenVault() *tokenVault {
	return &tokenVault{tokens: map[string]mintedToken{}}
}

func (v *tokenVault) put(projectID, token, label string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.tokens[projectID] = mintedToken{token: token, label: label, created: time.Now()}
}

// peek returns the token without consuming it, so a refresh during the connect
// step does not lose it before the operator has copied it.
func (v *tokenVault) peek(projectID string) (mintedToken, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	t, ok := v.tokens[projectID]
	if !ok {
		return mintedToken{}, false
	}
	// Expire rather than hold indefinitely: an abandoned wizard should not
	// leave a plaintext token in memory for the life of the process.
	if time.Since(t.created) > 30*time.Minute {
		delete(v.tokens, projectID)
		return mintedToken{}, false
	}
	return t, true
}

func (v *tokenVault) clear(projectID string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.tokens, projectID)
}

// wizardConnect renders the step that wires a repo to the project.
func (s *Server) wizardConnect(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.FormValue("project"))
	if projectID == "" {
		http.Redirect(w, r, "/onboard/name", http.StatusSeeOther)
		return
	}

	data := s.wizardData("connect", projectID)
	data["DaemonAddr"] = s.agentDaemonAddr
	data["CanMint"] = s.tokens != nil
	// Prefilled from step 1 when that resolved to a local directory: a URL is
	// not somewhere a file can be written.
	repoPath := strings.TrimSpace(r.FormValue("repo"))
	if repoPath != "" && !filepath.IsAbs(repoPath) {
		if src := ResolveRepo(repoPath); src.LocalPath != "" {
			repoPath = src.LocalPath
		} else {
			repoPath = ""
		}
	}
	data["RepoPath"] = repoPath
	data["Flash"] = r.URL.Query().Get("flash")
	data["FlashErr"] = r.URL.Query().Get("err")

	// A token minted a moment ago is shown here and nowhere else.
	if t, ok := s.vault.peek(projectID); ok {
		data["Token"] = t.token
		data["TokenLabel"] = t.label
	}

	// Preview the config either way, so the operator can copy it by hand
	// without ever naming a repo path.
	rendered, err := RenderMCPConfig(projectID, s.agentDaemonAddr, s.mcpBinary)
	if err != nil {
		s.fail(w, "connect", err)
		return
	}
	data["Config"] = rendered

	// When a path is given, show exactly what writing there would do before
	// offering the button that does it.
	if repo, _ := data["RepoPath"].(string); repo != "" {
		plan, err := PlanMCPWrite(repo, projectID, s.agentDaemonAddr, s.mcpBinary)
		if err != nil {
			data["PlanErr"] = err.Error()
		} else {
			data["Plan"] = plan
			data["Config"] = plan.Content
		}
	}

	s.render(w, "wizard_connect.html", data)
}

// wizardMint issues a token for the project.
func (s *Server) wizardMint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	projectID := strings.TrimSpace(r.FormValue("project"))
	if projectID == "" {
		redirectErr(w, r, "/onboard/name", "project required")
		return
	}
	if s.tokens == nil {
		redirectErr(w, r, "/onboard/connect?project="+urlEscape(projectID),
			"no token store configured")
		return
	}

	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		label = "agent"
	}

	// Read-write: this is the token the repo's agents use to record what they
	// learn, so onboarding would be pointless if it could not write. A read-only
	// token is minted deliberately from the project page instead.
	raw, err := s.tokens.MintToken(r.Context(), projectID, label, actorFrom(r), false)
	if err != nil {
		redirectErr(w, r, "/onboard/connect?project="+urlEscape(projectID), err.Error())
		return
	}
	// Stash server-side; the redirect carries only the project, never the token.
	s.vault.put(projectID, raw, label)

	http.Redirect(w, r, "/onboard/connect?project="+urlEscape(projectID)+
		"&repo="+urlEscape(strings.TrimSpace(r.FormValue("repo"))), http.StatusSeeOther)
}

// wizardWrite writes .mcp.json into the named repo.
func (s *Server) wizardWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	projectID := strings.TrimSpace(r.FormValue("project"))
	repo := strings.TrimSpace(r.FormValue("repo"))
	back := "/onboard/connect?project=" + urlEscape(projectID) + "&repo=" + urlEscape(repo)

	if projectID == "" || repo == "" {
		redirectErr(w, r, back, "project and repository path are required")
		return
	}

	// Re-plan rather than trusting a posted body: the form could have been
	// tampered with, and the plan is what decides the destination path.
	plan, err := PlanMCPWrite(repo, projectID, s.agentDaemonAddr, s.mcpBinary)
	if err != nil {
		redirectErr(w, r, back, err.Error())
		return
	}
	// Replacing an existing "silo" entry needs its own confirmation: the
	// operator may be pointing at the wrong repo, and the existing entry may
	// name a different project.
	if plan.Conflict && r.FormValue("confirm_replace") != "yes" {
		redirectErr(w, r, back, "this repo already has a \"silo\" server configured; "+
			"tick the replace box to overwrite it")
		return
	}

	if err := WriteMCPConfig(plan); err != nil {
		redirectErr(w, r, back, err.Error())
		return
	}

	msg := "wrote " + plan.Path
	if len(plan.OtherServers) > 0 {
		msg += " (kept " + strings.Join(plan.OtherServers, ", ") + ")"
	}
	redirectFlash(w, r, back, msg)
}
