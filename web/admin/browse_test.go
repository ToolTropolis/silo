package admin

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This endpoint reads the operator's filesystem, so escaping the home directory
// is the failure that matters. Every one of these must be refused.
func TestWithinRoots_RefusesEscapes(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}

	outside := []string{
		"/etc",
		"/etc/passwd",
		"/",
		filepath.Join(home, "..", "..", "etc"),
		home + "-other", // the separator check: a sibling must not match by prefix
	}
	for _, p := range outside {
		if withinRoots(p) {
			t.Errorf("withinRoots(%q) = true, want false — that is outside the home directory", p)
		}
	}

	inside := []string{home, filepath.Join(home, "code"), filepath.Join(home, "a", "b", "c")}
	for _, p := range inside {
		if !withinRoots(p) {
			t.Errorf("withinRoots(%q) = false, want true", p)
		}
	}
}

// A traversal that lands back inside the root is fine — it is only an escape
// that matters, and Clean resolves it before the check.
func TestWithinRoots_NormalizesTraversalWithin(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	p := filepath.Join(home, "code", "..", "docs")
	if !withinRoots(p) {
		t.Errorf("withinRoots(%q) = false; a path that normalizes back inside is allowed", p)
	}
}

// A symlink pointing outside the home directory must not become a way to read
// there — a prefix check alone would let it through.
func TestWithinRoots_RefusesSymlinkEscape(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	// Build the link inside a temp dir under the home directory, so the test
	// cleans up after itself.
	base, err := os.MkdirTemp(home, ".silo-browse-test-")
	if err != nil {
		t.Skipf("cannot create a test directory under home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	link := filepath.Join(base, "escape")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}

	if withinRoots(link) {
		t.Error("a symlink to /etc must not be browsable just because it sits under home")
	}
}

func TestListDirs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha", "beta", ".hidden"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// A repo, to check it sorts first and is marked.
	repo := filepath.Join(dir, "zeta-repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "afile.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	entries, err := listDirs(dir)
	if err != nil {
		t.Fatalf("listDirs: %v", err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	// Repos first — the operator is looking for one.
	if len(entries) == 0 || entries[0].Name != "zeta-repo" {
		t.Errorf("entries = %v, want the repository first", names)
	}
	if !entries[0].IsRepo {
		t.Error("a directory containing .git should be marked as a repo")
	}
	for _, n := range names {
		if strings.HasPrefix(n, ".") {
			t.Errorf("hidden directory %q should not be listed", n)
		}
		if n == "afile.txt" {
			t.Error("files should not be listed; this browses directories")
		}
	}
	if len(entries) != 3 {
		t.Errorf("got %d entries, want 3 (alpha, beta, zeta-repo)", len(entries))
	}
}

func TestListDirs_MissingDirectory(t *testing.T) {
	if _, err := listDirs("/nonexistent/definitely/not/here"); err == nil {
		t.Error("listing a missing directory should error")
	}
}

func TestCrumbsFor(t *testing.T) {
	root := filepath.Join("/home", "user")
	crumbs := crumbsFor(filepath.Join(root, "code", "myrepo"), root)

	if len(crumbs) != 3 {
		t.Fatalf("got %d crumbs, want 3", len(crumbs))
	}
	if crumbs[0].Name != "~" || crumbs[0].Path != root {
		t.Errorf("first crumb = %+v, want the root", crumbs[0])
	}
	if crumbs[2].Name != "myrepo" {
		t.Errorf("last crumb = %q, want myrepo", crumbs[2].Name)
	}
	// Each crumb must be navigable to on its own.
	if crumbs[1].Path != filepath.Join(root, "code") {
		t.Errorf("crumb path = %q", crumbs[1].Path)
	}

	// At the root there is only the root.
	if got := crumbsFor(root, root); len(got) != 1 {
		t.Errorf("got %d crumbs at the root, want 1", len(got))
	}
}

// The browse page has to render and offer navigation.
func TestBrowse_ListsAndMarksRepos(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	base, err := os.MkdirTemp(home, ".silo-browse-test-")
	if err != nil {
		t.Skipf("cannot create a test directory under home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	repo := filepath.Join(base, "myrepo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})
	body := getBody(t, ts, "/browse?dir="+url.QueryEscape(base))

	if !strings.Contains(body, "myrepo") {
		t.Error("the directory listing should show the repo")
	}
	if !strings.Contains(body, "git repo") {
		t.Error("a repository should be marked")
	}
	// Selecting hands the path back to step 1.
	if !strings.Contains(body, "/onboard/name?repo=") {
		t.Error("selecting a directory should return to the naming step with the path")
	}
}

// Browsing outside the home directory must be refused, and explained rather
// than silently redirected.
func TestBrowse_RefusesOutsideHome(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/browse?dir=%2Fetc")
	if !strings.Contains(body, "outside your home directory") {
		t.Error("browsing outside home should be refused with an explanation")
	}
	// It must not leak a listing of the refused directory.
	if strings.Contains(body, "passwd") {
		t.Error("the refused directory was listed anyway")
	}
}

func TestBrowse_DefaultsToHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/browse")
	if !strings.Contains(body, "Find a repository") {
		t.Error("the browse page should render with no dir given")
	}
}

// The wizard has to offer the browser, or it is unreachable.
func TestWizard_OffersBrowse(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/onboard/name")
	if !strings.Contains(body, `href="/browse"`) {
		t.Error("step 1 should link to the directory browser")
	}
}
