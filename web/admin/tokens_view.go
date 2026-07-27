package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tooltropolis/silo/internal/registry"
)

// tokenRow is one agent token, ready to render. The token itself is never here
// — only a hash prefix, which is enough to tell two tokens apart without being
// usable as a credential.
type tokenRow struct {
	Hash      string
	Display   string
	Label     string
	CreatedAt string
	CreatedBy string
	LastUsed  string
	Revoked   bool
	RevokedAt string
}

// handleProject renders one project: its tokens, its cache, and its lifecycle.
//
// Tokens get their own page rather than a column on the projects list. They are
// credentials with their own lifecycle — minted, used, revoked — and revoking
// the wrong one is the kind of mistake a cramped nested table invites.
func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		s.fail(w, "project", errors.New("no registry configured"))
		return
	}
	projectID := strings.TrimSpace(r.FormValue("project"))
	if projectID == "" {
		http.Redirect(w, r, "/projects", http.StatusSeeOther)
		return
	}
	ctx := r.Context()

	rec, err := s.registry.Get(ctx, projectID)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			redirectErr(w, r, "/projects", "no such project: "+projectID)
			return
		}
		s.fail(w, "project", err)
		return
	}

	data := map[string]any{
		"Active":       "projects",
		"Subtitle":     "Agent tokens and lifecycle for this project",
		"Project":      projectID,
		"Record":       rec,
		"CanMint":      s.tokens != nil,
		"DashboardURL": s.dashboardURL,
		"Flash":        r.URL.Query().Get("flash"),
		"FlashErr":     r.URL.Query().Get("err"),
	}

	// A token minted from this page is revealed here, once.
	if t, ok := s.vault.peek(projectID); ok {
		data["Token"] = t.token
		data["TokenLabel"] = t.label
	}

	if s.tokens != nil {
		tokens, err := s.tokens.ListTokens(ctx, projectID)
		if err != nil {
			data["TokensErr"] = err.Error()
		} else {
			rows := make([]tokenRow, 0, len(tokens))
			live := 0
			for _, t := range tokens {
				if !t.Revoked() {
					live++
				}
				rows = append(rows, tokenRow{
					Hash:      t.Hash,
					Display:   t.Display(),
					Label:     t.Label,
					CreatedAt: t.CreatedAt,
					CreatedBy: t.CreatedBy,
					LastUsed:  t.LastUsedAt,
					Revoked:   t.Revoked(),
					RevokedAt: t.RevokedAt,
				})
			}
			data["Tokens"] = rows
			data["LiveTokens"] = live
		}
	}

	// Agents defined in the linked repo. Read-only: it answers "what uses this
	// project, and is any of it wired up?" without the console reaching into a
	// repo it does not own.
	if rec.RepoPath != "" {
		scan := ScanAgents(rec.RepoPath)
		data["Agents"] = scan.Agents
		data["AgentDir"] = scan.Dir
		data["AgentErr"] = scan.Problem
		data["MCPConfigured"] = hasMCPConfig(rec.RepoPath)
	}

	// The .mcp.json for this project, so the page is self-sufficient for
	// re-wiring a repo without walking back through the wizard.
	if cfg, err := RenderMCPConfig(projectID, s.agentDaemonAddr, s.mcpBinary); err == nil {
		data["Config"] = cfg
	}

	s.render(w, "project.html", data)
}

// handleRevokeToken kills one token.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	projectID := strings.TrimSpace(r.FormValue("project"))
	hash := strings.TrimSpace(r.FormValue("hash"))
	back := "/project?project=" + urlEscape(projectID)

	if projectID == "" || hash == "" {
		redirectErr(w, r, back, "project and token are required")
		return
	}
	if s.tokens == nil {
		redirectErr(w, r, back, "no token store configured")
		return
	}

	// Confirm the token belongs to this project before revoking it. The hash
	// arrives from a form, and revoking another project's token because a field
	// was tampered with would be a cross-project action from a per-project page.
	tokens, err := s.tokens.ListTokens(r.Context(), projectID)
	if err != nil {
		redirectErr(w, r, back, err.Error())
		return
	}
	found := false
	for _, t := range tokens {
		if t.Hash == hash {
			found = true
			break
		}
	}
	if !found {
		redirectErr(w, r, back, "that token does not belong to this project")
		return
	}

	if err := s.tokens.RevokeToken(r.Context(), hash); err != nil {
		redirectErr(w, r, back, err.Error())
		return
	}
	// Daemons cache verified tokens, so a revocation takes effect within their
	// token-cache TTL rather than instantly. Say so instead of implying the
	// token is dead everywhere the moment the page reloads.
	redirectFlash(w, r, back, "revoked "+hash[:12]+
		" — daemons stop accepting it within their token-cache TTL")
}

// handleMintToken issues a token from the project page.
func (s *Server) handleMintToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	projectID := strings.TrimSpace(r.FormValue("project"))
	back := "/project?project=" + urlEscape(projectID)

	if projectID == "" {
		redirectErr(w, r, "/projects", "project required")
		return
	}
	if s.tokens == nil {
		redirectErr(w, r, back, "no token store configured")
		return
	}

	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		label = "agent"
	}

	raw, err := s.tokens.MintToken(r.Context(), projectID, label, actorFrom(r))
	if err != nil {
		redirectErr(w, r, back, err.Error())
		return
	}
	// Stashed server-side; the redirect carries only the project.
	s.vault.put(projectID, raw, label)
	http.Redirect(w, r, back, http.StatusSeeOther)
}
