package dashboard

import (
	"errors"
	"net/http"
)

// handleMemory serves the memory version browser (spec §7.2). It progressively
// narrows: no project → project picker; project → its memory paths; path →
// version history; version → that version's content.
//
// Content is decrypted server-side by the backend adapter; ciphertext and key
// material are never sent to the browser.
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		s.fail(w, "Memory", errors.New("no backend configured (pass --backend-endpoint)"))
		return
	}
	ctx := r.Context()
	project := r.URL.Query().Get("project")
	path := r.URL.Query().Get("path")
	versionID := r.URL.Query().Get("version")

	data := map[string]any{"Active": "memory", "Project": project, "Path": path, "VersionID": versionID}

	// No project selected — offer the list from the registry.
	if project == "" {
		projects, err := s.projectIDs(r)
		if err != nil {
			s.fail(w, "Memory", err)
			return
		}
		data["Projects"] = projects
		s.render(w, "memory.html", data)
		return
	}

	// Project selected — list its memory paths (excluding internal namespaces).
	paths, err := s.memory.ListPaths(ctx, project, "")
	if err != nil {
		s.fail(w, "Memory", err)
		return
	}
	data["Paths"] = filterMemoryPaths(paths)

	if path == "" {
		s.render(w, "memory.html", data)
		return
	}

	// Path selected — show its version history, newest first.
	versions, err := s.memory.ListVersions(ctx, project, path)
	if err != nil {
		s.fail(w, "Memory", err)
		return
	}
	data["Versions"] = versions

	// Version selected (or latest) — render that version's content.
	content, ver, err := s.memory.Get(ctx, project, path, versionID)
	if err != nil {
		s.fail(w, "Memory", err)
		return
	}
	data["Content"] = string(content)
	data["ShownVersion"] = ver
	s.render(w, "memory.html", data)
}

// projectIDs lists project IDs from the registry for the picker.
func (s *Server) projectIDs(r *http.Request) ([]string, error) {
	if s.registry == nil {
		return nil, errors.New("no registry configured, so projects can't be listed")
	}
	records, err := s.registry.List(r.Context())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(records))
	for _, rec := range records {
		ids = append(ids, rec.ProjectID)
	}
	return ids, nil
}

// filterMemoryPaths drops Silo's internal namespaces so the browser shows
// memory, not Distilator output or raw session transcripts. Those are visible
// through the Distilator review view instead.
func filterMemoryPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if isInternalPath(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}
