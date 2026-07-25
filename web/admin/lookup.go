package admin

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxLookupDepth bounds how deep the search descends below the home directory.
//
// Repositories live a few levels down (~/code/org/repo), not twenty. A bound
// keeps a stray deep tree — node_modules, a Time Machine mount — from turning a
// click into a filesystem walk.
const maxLookupDepth = 6

// maxLookupMatches stops a very common folder name (say "src") from returning
// hundreds of rows nobody will read.
const maxLookupMatches = 25

// lookupTimeout bounds the whole walk. A slow network mount must not hang the
// request; a partial answer plus the manual browser beats an unresponsive page.
const lookupTimeout = 4 * time.Second

// folderMatch is one directory whose name matched.
type folderMatch struct {
	Path   string
	IsRepo bool
	// Depth orders results so the shallowest — usually the obvious one — is
	// offered first.
	Depth int
}

// handleLookup resolves a folder name to real paths under the home directory.
//
// This is the half of the native picker the browser cannot do. `webkitdirectory`
// opens the genuine OS dialog but hands the page only the folder's *name*: MDN
// is explicit that "the absolute path of the chosen directory is never exposed
// to the page". Since the console runs on the operator's machine, it can turn
// that name back into a path.
//
// One match resolves silently; several are offered; none falls back to the
// manual browser.
func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	roots := browseRoots()

	data := map[string]any{
		"Active":   "projects",
		"Subtitle": "Match the folder you picked",
		"Name":     name,
	}

	if name == "" || len(roots) == 0 {
		http.Redirect(w, r, "/browse", http.StatusSeeOther)
		return
	}
	// The name comes from a browser, so treat it as untrusted input rather than
	// a path fragment: a value containing separators must never be joined onto
	// a root and walked.
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		data["Matches"] = nil
		data["Problem"] = "That does not look like a folder name."
		s.render(w, "lookup.html", data)
		return
	}

	matches, truncated := findFolders(roots[0], name)
	data["Matches"] = matches
	data["Truncated"] = truncated

	switch len(matches) {
	case 0:
		data["Problem"] = "No folder called " + name + " under your home directory. " +
			"Browse to it, or type the full path."
	case 1:
		// Unambiguous: go straight on, which is what makes this feel like a
		// native picker rather than a two-step search.
		http.Redirect(w, r, "/onboard/name?repo="+urlEscape(matches[0].Path), http.StatusSeeOther)
		return
	}
	s.render(w, "lookup.html", data)
}

// findFolders walks the home directory for directories with the given name.
//
// Reports whether the result was truncated, so the caller can say so rather
// than presenting a partial list as complete.
func findFolders(root, name string) ([]folderMatch, bool) {
	deadline := time.Now().Add(lookupTimeout)
	var out []folderMatch
	truncated := false

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is skipped, not fatal: a home directory
			// usually contains several the operator cannot enter.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if len(out) >= maxLookupMatches || time.Now().After(deadline) {
			truncated = true
			return filepath.SkipAll
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fs.SkipDir
		}
		depth := 0
		if rel != "." {
			depth = strings.Count(rel, string(os.PathSeparator)) + 1
		}

		base := d.Name()
		// Skip hidden trees and the usual dependency graveyards: a repo is not
		// inside node_modules, and walking one costs seconds.
		if depth > 0 && (strings.HasPrefix(base, ".") || isNoiseDir(base)) {
			return fs.SkipDir
		}
		if depth > maxLookupDepth {
			return fs.SkipDir
		}
		if base == name && depth > 0 {
			out = append(out, folderMatch{Path: path, IsRepo: isGitRepo(path), Depth: depth})
			// A repository is never nested inside another repository here, and
			// descending would only find its subdirectories.
			return fs.SkipDir
		}
		return nil
	})

	// Repos first, then shallowest: both are proxies for "the one you meant".
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsRepo != out[j].IsRepo {
			return out[i].IsRepo
		}
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		return out[i].Path < out[j].Path
	})
	return out, truncated
}

// isNoiseDir reports whether a directory is a dependency or build tree worth
// skipping. Not exhaustive — just the ones big enough to matter.
func isNoiseDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "target", "dist", "build", "venv", "__pycache__",
		"Library", "Applications", "Pictures", "Music", "Movies":
		return true
	}
	return false
}
