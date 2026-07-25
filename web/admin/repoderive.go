package admin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tooltropolis/silo/internal/project"
)

// RepoSource is what an operator typed in the first step, resolved.
//
// Onboarding is about giving a repository memory, so the wizard asks for the
// repository. The project ID is an implementation detail of the storage layer —
// it becomes a bucket name and a cache filename — and deriving it means the
// operator names the thing they are actually thinking about.
type RepoSource struct {
	// Input is exactly what was typed, kept for redisplay.
	Input string
	// LocalPath is set when the input resolved to a directory on this machine.
	// Only then can the wizard read the repo's remote or write .mcp.json.
	LocalPath string
	// RemoteURL is the repo's origin, read from a local repo or supplied
	// directly.
	RemoteURL string
	// SuggestedID is the derived project ID, already normalized to something
	// ValidateID accepts.
	SuggestedID string
	// RawName is the repo name before normalization. Shown alongside
	// SuggestedID when they differ, so a transformation is never silent.
	RawName string
	// Normalized reports whether the name had to be changed to be usable.
	Normalized bool
	// IsGitRepo is false for a directory with no .git — allowed, but worth
	// saying.
	IsGitRepo bool
	// Problem explains why nothing could be derived.
	Problem string
}

// Derived reports whether a usable project ID came out of the input.
func (r RepoSource) Derived() bool { return r.SuggestedID != "" }

// ResolveRepo works out what an operator meant by a path or a URL.
//
// Accepts either, because the console does not always run on the machine that
// holds the repo. A local path unlocks more — reading the origin remote, and
// writing .mcp.json later in the flow — but a URL is enough to name a project.
func ResolveRepo(input string) RepoSource {
	src := RepoSource{Input: strings.TrimSpace(input)}
	if src.Input == "" {
		return src
	}

	switch {
	case looksLikeURL(src.Input):
		src.RemoteURL = src.Input
		src.RawName = repoNameFromURL(src.Input)
	default:
		src.resolveLocal()
	}

	if src.RawName == "" {
		if src.Problem == "" {
			src.Problem = "could not work out a repository name from that"
		}
		return src
	}

	src.SuggestedID = NormalizeProjectID(src.RawName)
	// Compared against the raw name, not a lowercased one: folding "MyService"
	// to "myservice" is a real change, and an operator who does not notice it
	// will look for a bucket that is not there.
	src.Normalized = src.SuggestedID != src.RawName
	if src.SuggestedID != "" {
		if err := project.ValidateID(src.SuggestedID); err != nil {
			// Normalization could not rescue it — e.g. a name that is entirely
			// punctuation, or too short even after cleaning.
			src.Problem = "the repository name cannot be turned into a valid project ID (" +
				err.Error() + "). Enter one yourself."
			src.SuggestedID = ""
		}
	}
	return src
}

// resolveLocal inspects a directory on this machine.
func (r *RepoSource) resolveLocal() {
	path := r.Input
	if strings.HasPrefix(path, "~") {
		// Expanding ~ would guess whose home directory is meant; the console may
		// not run as the user who owns the repo.
		r.Problem = "give an absolute path rather than one starting with ~"
		return
	}
	if !filepath.IsAbs(path) {
		r.Problem = "give an absolute path (e.g. /Users/you/code/myrepo) or a repository URL"
		return
	}
	path = filepath.Clean(path)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			r.Problem = "no such directory: " + path
			return
		}
		r.Problem = "cannot read " + path + ": " + err.Error()
		return
	}
	if !info.IsDir() {
		r.Problem = path + " is a file, not a directory"
		return
	}

	r.LocalPath = path
	r.RawName = filepath.Base(path)

	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		r.IsGitRepo = true
		// Prefer the remote's name: a checkout directory is often renamed
		// locally, and the remote is the repository's real identity.
		if url := gitOriginURL(path); url != "" {
			r.RemoteURL = url
			if name := repoNameFromURL(url); name != "" {
				r.RawName = name
			}
		}
	}
}

// gitOriginURL reads a repo's origin, or "" if there isn't one.
//
// Bounded and non-fatal: a repo with no remote, a git that hangs on a network
// filesystem, or no git at all must all leave the wizard usable.
func gitOriginURL(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// looksLikeURL reports whether the input is a remote rather than a path.
func looksLikeURL(s string) bool {
	for _, prefix := range []string{"http://", "https://", "ssh://", "git://", "git@"} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// repoNameFromURL pulls the repository name out of a clone URL, covering the
// HTTPS, SSH, and scp-like forms git accepts.
func repoNameFromURL(url string) string {
	s := strings.TrimSpace(url)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")

	// scp-like: git@github.com:org/repo
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i:], "/") {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// NormalizeProjectID turns a repository name into something ValidateID accepts.
//
// Repository names are far more permissive than project IDs, which have to work
// as both an S3 bucket component and a filename: "MyService", "my_service", and
// "api.v2" are all ordinary repo names and all invalid here. Rather than
// rejecting them, transform them — and the caller shows what changed, because
// the result becomes a bucket name that can never be renamed.
func NormalizeProjectID(name string) string {
	var b strings.Builder
	lastHyphen := false

	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case r == '-' || r == '_' || r == '.' || r == ' ':
			// Collapse runs of separators: "my..cool__thing" would otherwise
			// become "my--cool--thing", which ValidateID rejects.
			if !lastHyphen && b.Len() > 0 {
				b.WriteRune('-')
				lastHyphen = true
			}
		default:
			// Anything else (unicode, punctuation) is dropped rather than
			// transliterated: a guess at what "café" should become is worse
			// than letting the operator decide.
		}
	}

	out := strings.Trim(b.String(), "-")
	if len(out) > project.MaxIDLen {
		out = strings.Trim(out[:project.MaxIDLen], "-")
	}
	return out
}

// repoLabel shortens a clone URL to the "org/repo" form people use when talking
// about a repository. Falls back to the input when it does not parse that way,
// rather than showing nothing.
func repoLabel(url string) string {
	s := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(url), "/"), ".git")
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s = strings.TrimPrefix(s, "git@")
	// Drop the host: "github.com:org/repo" and "github.com/org/repo" both
	// become "org/repo".
	if i := strings.IndexAny(s, ":/"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return url
	}
	return s
}
