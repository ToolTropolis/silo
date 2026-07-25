package admin

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// homeTemp makes a scratch directory inside the home directory, since lookup is
// confined there.
func homeTemp(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	// Deliberately NOT hidden: the walk skips dot-directories, so a test
	// fixture under one would be invisible to the handler it is testing.
	dir, err := os.MkdirTemp(home, "silo-lookup-test-")
	if err != nil {
		t.Skipf("cannot create a test directory under home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func mkRepo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir repo %s: %v", path, err)
	}
}

func TestFindFolders_MatchesByName(t *testing.T) {
	base := homeTemp(t)
	mkRepo(t, filepath.Join(base, "code", "wanted"))
	if err := os.MkdirAll(filepath.Join(base, "other", "unwanted"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	matches, _ := findFolders(base, "wanted")
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if !strings.HasSuffix(matches[0].Path, filepath.Join("code", "wanted")) {
		t.Errorf("Path = %q", matches[0].Path)
	}
	if !matches[0].IsRepo {
		t.Error("a directory with .git should be marked as a repo")
	}
}

// Several folders can share a name; the operator picks. Repos come first
// because that is what they are looking for.
func TestFindFolders_OrdersReposFirstThenShallowest(t *testing.T) {
	base := homeTemp(t)
	if err := os.MkdirAll(filepath.Join(base, "shallow", "myrepo"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mkRepo(t, filepath.Join(base, "a", "b", "c", "myrepo"))

	matches, _ := findFolders(base, "myrepo")
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(matches))
	}
	if !matches[0].IsRepo {
		t.Error("the git repo should be offered first even though it is deeper")
	}
	if matches[1].IsRepo {
		t.Error("the plain directory should come second")
	}
}

// A deep tree must not be walked forever, and dependency directories are never
// where a repo lives.
func TestFindFolders_SkipsNoiseAndHiddenTrees(t *testing.T) {
	base := homeTemp(t)
	for _, p := range []string{
		filepath.Join(base, "node_modules", "target"),
		filepath.Join(base, ".cache", "target"),
		filepath.Join(base, "vendor", "target"),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	matches, _ := findFolders(base, "target")
	if len(matches) != 0 {
		t.Errorf("got %+v, want nothing from node_modules, vendor, or hidden trees", matches)
	}
}

func TestFindFolders_RespectsDepthLimit(t *testing.T) {
	base := homeTemp(t)
	deep := base
	for range maxLookupDepth + 3 {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(filepath.Join(deep, "buried"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	matches, _ := findFolders(base, "buried")
	if len(matches) != 0 {
		t.Errorf("a folder below the depth limit should not be found, got %+v", matches)
	}
}

// A repository is not nested inside another here, so descending into a match
// would only surface its own subdirectories.
func TestFindFolders_DoesNotDescendIntoAMatch(t *testing.T) {
	base := homeTemp(t)
	mkRepo(t, filepath.Join(base, "outer"))
	if err := os.MkdirAll(filepath.Join(base, "outer", "outer"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	matches, _ := findFolders(base, "outer")
	if len(matches) != 1 {
		t.Errorf("got %d matches, want 1 — the search should not descend into a match", len(matches))
	}
}

// One match is the common case and should feel like a native picker: straight
// through, no extra page.
func TestLookup_SingleMatchGoesStraightToNaming(t *testing.T) {
	base := homeTemp(t)
	unique := "silo-lookup-unique-" + filepath.Base(base)
	mkRepo(t, filepath.Join(base, unique))

	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})
	resp, err := noRedirectClient().Get(ts.URL + "/lookup?name=" + url.QueryEscape(unique))
	if err != nil {
		t.Fatalf("GET /lookup: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 303 {
		t.Fatalf("status = %d, want a redirect for an unambiguous match", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/onboard/name?repo=") {
		t.Errorf("Location = %q, want the naming step", loc)
	}
	if !strings.Contains(loc, url.QueryEscape(unique)) {
		t.Errorf("Location = %q, want it to carry the resolved path", loc)
	}
}

func TestLookup_AmbiguousOffersAChoice(t *testing.T) {
	base := homeTemp(t)
	name := "dup-" + filepath.Base(base)
	mkRepo(t, filepath.Join(base, "one", name))
	mkRepo(t, filepath.Join(base, "two", name))

	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})
	body := getBody(t, ts, "/lookup?name="+url.QueryEscape(name))

	if !strings.Contains(body, "Which") {
		t.Error("an ambiguous name should ask which one")
	}
	if strings.Count(body, "Use this") < 2 {
		t.Error("both matches should be selectable")
	}
}

// No match must explain itself and offer the manual routes, not dead-end.
func TestLookup_NoMatchFallsBack(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/lookup?name=definitely-not-a-real-folder-xyz")
	if !strings.Contains(body, "No folder called") {
		t.Error("a missing folder should be explained")
	}
	if !strings.Contains(body, "/browse") {
		t.Error("the browser should be offered as a fallback")
	}
}

// The name arrives from a browser, so it is untrusted: a value containing
// separators must never be joined onto a root and walked.
func TestLookup_RejectsPathLikeNames(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})

	for _, bad := range []string{"../etc", "a/b", ".."} {
		body := getBody(t, ts, "/lookup?name="+url.QueryEscape(bad))
		if !strings.Contains(body, "does not look like a folder name") {
			t.Errorf("name %q should be rejected as path-like", bad)
		}
	}
}

func TestLookup_EmptyNameRedirectsToBrowse(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})

	resp, err := noRedirectClient().Get(ts.URL + "/lookup")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/browse" {
		t.Errorf("empty name should redirect to /browse, got %d %q",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

// The picker is an enhancement: the page must still work with scripting off,
// which means the Browse… link keeps its href.
func TestWizard_BrowseLinkWorksWithoutJS(t *testing.T) {
	ts := newFixture(t, Config{Registry: &fakeRegistry{}, Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/onboard/name")
	if !strings.Contains(body, `id="browse-link" href="/browse"`) {
		t.Error("the Browse link must have a real href so it works without JS")
	}
	if !strings.Contains(body, "webkitdirectory") {
		t.Error("the native picker input should be present for browsers that support it")
	}
	if !strings.Contains(body, `action="/lookup"`) {
		t.Error("the picker should submit the folder name to the lookup endpoint")
	}
}
