package admin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tooltropolis/silo/internal/project"
)

// Repo names are far more permissive than project IDs. Every normalized result
// must be something ValidateID accepts, or the wizard would suggest an ID that
// cannot be onboarded.
func TestNormalizeProjectID(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"already valid", "silo", "silo"},
		{"already valid with hyphen", "my-service", "my-service"},
		{"uppercase is folded", "MyService", "myservice"},
		{"screaming case", "SCREAMING", "screaming"},
		{"underscores become hyphens", "my_service", "my-service"},
		{"dots become hyphens", "api.v2", "api-v2"},
		{"spaces become hyphens", "my cool repo", "my-cool-repo"},
		{"runs of separators collapse", "my..cool__thing", "my-cool-thing"},
		{"leading and trailing separators are trimmed", "-repo-", "repo"},
		{"mixed punctuation", "My_Awesome.Project", "my-awesome-project"},
		{"digits survive", "repo123", "repo123"},
		{"unicode is dropped rather than guessed at", "café", "caf"},
		{"nothing usable", "!!!", ""},
		{"empty", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeProjectID(tc.in)
			if got != tc.want {
				t.Errorf("NormalizeProjectID(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// The contract that matters: anything non-empty must be onboardable.
			if got != "" {
				if err := project.ValidateID(got); err != nil {
					t.Errorf("NormalizeProjectID(%q) = %q, which ValidateID rejects: %v",
						tc.in, got, err)
				}
			}
		})
	}
}

// A very long repo name must be truncated to something valid rather than
// producing an ID that fails at onboarding.
func TestNormalizeProjectID_TruncatesLongNames(t *testing.T) {
	long := strings.Repeat("verylongname", 20)
	got := NormalizeProjectID(long)

	if len(got) > project.MaxIDLen {
		t.Errorf("result is %d chars, want <= %d", len(got), project.MaxIDLen)
	}
	if err := project.ValidateID(got); err != nil {
		t.Errorf("truncated result %q is invalid: %v", got, err)
	}
	// Truncation must not leave a trailing hyphen, which ValidateID rejects.
	if strings.HasSuffix(got, "-") {
		t.Errorf("truncated result %q ends in a hyphen", got)
	}
}

func TestRepoNameFromURL(t *testing.T) {
	tests := []struct{ url, want string }{
		{"https://github.com/ToolTropolis/silo.git", "silo"},
		{"https://github.com/ToolTropolis/silo", "silo"},
		{"git@github.com:ToolTropolis/silo.git", "silo"},
		{"ssh://git@github.com/org/my-service.git", "my-service"},
		{"git://example.com/repo.git", "repo"},
		{"https://gitlab.com/group/subgroup/project.git", "project"},
		{"https://github.com/org/repo/", "repo"},
	}
	for _, tc := range tests {
		if got := repoNameFromURL(tc.url); got != tc.want {
			t.Errorf("repoNameFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestResolveRepo_FromURL(t *testing.T) {
	src := ResolveRepo("https://github.com/ToolTropolis/MyService.git")

	if !src.Derived() {
		t.Fatalf("expected an ID to be derived, got problem %q", src.Problem)
	}
	if src.SuggestedID != "myservice" {
		t.Errorf("SuggestedID = %q, want myservice", src.SuggestedID)
	}
	if !src.Normalized {
		t.Error("the name was changed, so Normalized should be true")
	}
	if src.RawName != "MyService" {
		t.Errorf("RawName = %q, want the original for display", src.RawName)
	}
	if src.RemoteURL == "" {
		t.Error("a URL input should be recorded as the remote")
	}
	if src.LocalPath != "" {
		t.Error("a URL is not a local path")
	}
}

// A name needing no change must not be reported as normalized, or the UI would
// warn about a transformation that did not happen.
func TestResolveRepo_UnchangedNameIsNotFlagged(t *testing.T) {
	src := ResolveRepo("https://github.com/org/my-service.git")

	if src.SuggestedID != "my-service" {
		t.Errorf("SuggestedID = %q", src.SuggestedID)
	}
	if src.Normalized {
		t.Error("nothing changed, so Normalized should be false")
	}
}

func TestResolveRepo_FromLocalGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "checkout-renamed-locally")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", "https://github.com/org/real-name.git")

	src := ResolveRepo(repo)

	if !src.IsGitRepo {
		t.Error("IsGitRepo should be true")
	}
	if src.LocalPath != repo {
		t.Errorf("LocalPath = %q, want %q", src.LocalPath, repo)
	}
	// The remote is the repository's real identity; a checkout directory is
	// often renamed locally.
	if src.SuggestedID != "real-name" {
		t.Errorf("SuggestedID = %q, want real-name from the remote", src.SuggestedID)
	}
	if src.RemoteURL == "" {
		t.Error("the origin URL should have been read")
	}
}

// A repo with no remote still yields a name from the directory.
func TestResolveRepo_LocalRepoWithoutRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "my_local_repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cmd := exec.Command("git", "-C", repo, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	src := ResolveRepo(repo)
	if src.SuggestedID != "my-local-repo" {
		t.Errorf("SuggestedID = %q, want the directory name normalized", src.SuggestedID)
	}
	if src.RemoteURL != "" {
		t.Errorf("RemoteURL = %q, want empty when there is no origin", src.RemoteURL)
	}
}

// A plain directory is allowed — it might be a repo about to be created — but
// the caller can say so.
func TestResolveRepo_NonRepoDirectory(t *testing.T) {
	dir := t.TempDir()
	src := ResolveRepo(dir)

	if !src.Derived() {
		t.Fatalf("a plain directory should still yield a name, got %q", src.Problem)
	}
	if src.IsGitRepo {
		t.Error("IsGitRepo should be false")
	}
}

func TestResolveRepo_RejectsBadInput(t *testing.T) {
	tests := []struct{ name, in, wantProblem string }{
		{"relative path", "relative/path", "absolute"},
		{"tilde", "~/code/repo", "absolute"},
		{"missing directory", "/nonexistent/definitely/not/here", "no such directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := ResolveRepo(tc.in)
			if src.Derived() {
				t.Errorf("%q should not derive an ID", tc.in)
			}
			if !strings.Contains(src.Problem, tc.wantProblem) {
				t.Errorf("Problem = %q, want it to mention %q", src.Problem, tc.wantProblem)
			}
		})
	}
}

func TestResolveRepo_EmptyInput(t *testing.T) {
	src := ResolveRepo("   ")
	if src.Derived() {
		t.Error("empty input should derive nothing")
	}
	if src.Problem != "" {
		t.Errorf("empty input is not an error to report yet, got %q", src.Problem)
	}
}

// A name that normalizes to something too short must be reported rather than
// suggested, since onboarding would reject it.
func TestResolveRepo_UnusableNameIsExplained(t *testing.T) {
	src := ResolveRepo("https://github.com/org/a.git")

	if src.Derived() {
		t.Errorf("SuggestedID = %q, want none for a name that cannot be valid", src.SuggestedID)
	}
	if src.Problem == "" {
		t.Error("the operator should be told why nothing was suggested")
	}
}

func TestLooksLikeURL(t *testing.T) {
	urls := []string{
		"https://github.com/org/repo", "http://x/y", "git@github.com:org/repo.git",
		"ssh://git@host/repo", "git://host/repo",
	}
	for _, u := range urls {
		if !looksLikeURL(u) {
			t.Errorf("looksLikeURL(%q) = false", u)
		}
	}
	paths := []string{"/Users/me/code/repo", "relative/path", "~/code", "C:/repo"}
	for _, p := range paths {
		if looksLikeURL(p) {
			t.Errorf("looksLikeURL(%q) = true", p)
		}
	}
}
