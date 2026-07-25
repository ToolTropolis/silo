package admin

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// browseEntry is one directory in the browser.
type browseEntry struct {
	Name string
	Path string
	// IsRepo marks a directory containing .git, so the operator can see at a
	// glance which entries are actually repositories.
	IsRepo bool
}

// browseCrumb is one segment of the current path, for navigating back up.
type browseCrumb struct {
	Name string
	Path string
}

// browseRoots returns the directories the browser may start from.
//
// A whitelist rather than "/" because this endpoint reads the operator's
// filesystem: confining it to a home directory means a bug in the path handling
// cannot enumerate somewhere sensitive, and there is no legitimate reason to
// onboard a repo from outside it.
func browseRoots() []string {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Clean(home))
	}
	return roots
}

// withinRoots reports whether path is inside one of the allowed roots.
//
// Compared after resolving symlinks: a symlink inside the home directory
// pointing at /etc would otherwise pass a prefix check and read outside the
// boundary. The lexical check runs first so a non-existent path still fails
// closed rather than erroring its way through.
func withinRoots(path string) bool {
	clean := filepath.Clean(path)
	roots := browseRoots()
	if len(roots) == 0 {
		return false
	}

	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		// Cannot resolve it (missing, or a broken link) — judge it lexically and
		// let the caller's Stat report the real problem.
		resolved = clean
	}

	for _, root := range roots {
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			resolvedRoot = root
		}
		if clean == root || resolved == resolvedRoot {
			return true
		}
		// The separator matters: without it "/home/user-other" would count as
		// inside "/home/user".
		if strings.HasPrefix(clean, root+string(os.PathSeparator)) &&
			strings.HasPrefix(resolved, resolvedRoot+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// handleBrowse lists directories so an operator can click to a repo rather than
// typing an absolute path.
//
// A browser's own file picker cannot do this: `<input type="file"
// webkitdirectory>` uploads files and deliberately withholds the absolute path.
// Since the console already runs on the operator's machine, listing server-side
// is the only way to turn a click into a path.
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	roots := browseRoots()
	if len(roots) == 0 {
		s.fail(w, "browse", errors.New("no home directory to browse from"))
		return
	}

	dir := strings.TrimSpace(r.FormValue("dir"))
	if dir == "" {
		dir = roots[0]
	}
	dir = filepath.Clean(dir)

	data := map[string]any{
		"Active":   "projects",
		"Subtitle": "Pick the repository to give memory",
		"Project":  strings.TrimSpace(r.FormValue("project")),
		"Root":     roots[0],
	}

	if !withinRoots(dir) {
		// Not an error page: send them back to the root with an explanation,
		// which is more useful than a dead end.
		data["Dir"] = roots[0]
		data["Warning"] = "That path is outside your home directory, so it cannot be browsed here. " +
			"Type it into the repository field instead."
		dir = roots[0]
	} else {
		data["Dir"] = dir
	}

	entries, err := listDirs(dir)
	if err != nil {
		data["Warning"] = err.Error()
		entries = nil
	}
	data["Entries"] = entries
	data["Crumbs"] = crumbsFor(dir, roots[0])
	data["IsRepo"] = isGitRepo(dir)
	if parent := filepath.Dir(dir); dir != roots[0] && withinRoots(parent) {
		data["Parent"] = parent
	}

	s.render(w, "browse.html", data)
}

// listDirs returns the visible subdirectories of dir, repos first.
func listDirs(dir string) ([]browseEntry, error) {
	f, err := os.ReadDir(dir)
	if err != nil {
		if os.IsPermission(err) {
			return nil, errors.New("no permission to read " + dir)
		}
		if os.IsNotExist(err) {
			return nil, errors.New("no such directory: " + dir)
		}
		return nil, err
	}

	out := make([]browseEntry, 0, len(f))
	for _, e := range f {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Hidden directories are noise when looking for a repo, and .git itself
		// is never the thing being selected.
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		out = append(out, browseEntry{Name: name, Path: path, IsRepo: isGitRepo(path)})
	}

	// Repositories first, then alphabetical: the operator is looking for a repo,
	// so surface them above ordinary folders.
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsRepo != out[j].IsRepo {
			return out[i].IsRepo
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// crumbsFor builds the path segments between the root and dir.
func crumbsFor(dir, root string) []browseCrumb {
	crumbs := []browseCrumb{{Name: "~", Path: root}}
	if dir == root {
		return crumbs
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return crumbs
	}
	cur := root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		crumbs = append(crumbs, browseCrumb{Name: part, Path: cur})
	}
	return crumbs
}
