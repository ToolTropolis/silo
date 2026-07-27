package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tooltropolis/silo/internal/registry"
)

func writeAgent(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestScanAgents_ReadsFrontmatter(t *testing.T) {
	repo := t.TempDir()
	writeAgent(t, filepath.Join(repo, "agents"), "reviewer.md",
		"---\nname: reviewer\ndescription: \"Reviews code. Second sentence dropped.\"\nmodel: sonnet\ncolor: green\n---\n\nBody here.\n")

	scan := ScanAgents(repo)
	if len(scan.Agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(scan.Agents))
	}
	a := scan.Agents[0]
	if a.Name != "reviewer" || a.Model != "sonnet" {
		t.Errorf("got %+v", a)
	}
	// Descriptions embed whole worked examples; a table cell needs one sentence.
	if !strings.HasPrefix(a.Description, "Reviews code.") || strings.Contains(a.Description, "Second sentence") {
		t.Errorf("Description = %q, want the first sentence only", a.Description)
	}
}

// The conventional locations differ across tools; all should be found.
func TestScanAgents_FindsConventionalDirs(t *testing.T) {
	for _, dir := range []string{".claude/agents", "agents", ".agents"} {
		repo := t.TempDir()
		writeAgent(t, filepath.Join(repo, dir), "a.md", "---\nname: a\n---\n")

		if scan := ScanAgents(repo); len(scan.Agents) != 1 {
			t.Errorf("%s: got %d agents, want 1", dir, len(scan.Agents))
		}
	}
}

// A file with no usable frontmatter still lists, named after the file — better
// than vanishing from an inventory whose job is completeness.
func TestScanAgents_FallsBackToFilename(t *testing.T) {
	repo := t.TempDir()
	writeAgent(t, filepath.Join(repo, "agents"), "no-frontmatter.md", "just prose\n")

	scan := ScanAgents(repo)
	if len(scan.Agents) != 1 || scan.Agents[0].Name != "no-frontmatter" {
		t.Errorf("got %+v, want the filename as the name", scan.Agents)
	}
}

func TestScanAgents_NoAgentsIsNotAnError(t *testing.T) {
	scan := ScanAgents(t.TempDir())
	if scan.Problem != "" {
		t.Errorf("Problem = %q, want empty for a repo with no agents", scan.Problem)
	}
	if len(scan.Agents) != 0 {
		t.Errorf("got %d agents", len(scan.Agents))
	}
}

func TestScanAgents_RejectsRelativePath(t *testing.T) {
	if scan := ScanAgents("relative/path"); scan.Problem == "" {
		t.Error("a relative repo path should be reported, not walked")
	}
}

// Nested keys belong to structures this parser does not understand; reading
// them as top-level fields would produce nonsense.
func TestParseAgentFrontmatter_IgnoresIndentedKeys(t *testing.T) {
	a := parseAgentFrontmatter("---\nname: real\nmetadata:\n  name: nested\n  model: wrong\n---\n")
	if a.Name != "real" {
		t.Errorf("Name = %q, want the top-level value", a.Name)
	}
	if a.Model != "" {
		t.Errorf("Model = %q, want empty — the only model key was nested", a.Model)
	}
}

// Wiring is per repo: one .mcp.json gives every agent the silo tools.
func TestHasMCPConfig(t *testing.T) {
	repo := t.TempDir()
	if hasMCPConfig(repo) {
		t.Error("no .mcp.json should read as unwired")
	}

	os.WriteFile(filepath.Join(repo, ".mcp.json"),
		[]byte(`{"mcpServers":{"github":{"command":"gh"}}}`), 0o644)
	if hasMCPConfig(repo) {
		t.Error("a .mcp.json without a silo server is not wired to Silo")
	}

	os.WriteFile(filepath.Join(repo, ".mcp.json"),
		[]byte(`{"mcpServers":{"silo":{"command":"silo-mcp"}}}`), 0o644)
	if !hasMCPConfig(repo) {
		t.Error("a silo server should read as wired")
	}
}

// An unwired repo is the case worth flagging: none of its agents can reach Silo.
func TestProject_FlagsUnwiredRepo(t *testing.T) {
	repo := t.TempDir()
	writeAgent(t, filepath.Join(repo, "agents"), "a.md", "---\nname: a\n---\n")

	ts := newFixture(t, Config{
		Registry: &fakeRegistry{records: []registry.ProjectRecord{
			{ProjectID: "proj-11", BucketName: "silo-proj-11", Status: registry.StatusActive, RepoPath: repo},
		}},
		Settings: newFakeSettings(nil),
	})

	body := getBody(t, ts, "/project?project=proj-11")
	if !strings.Contains(body, "None of these agents can reach Silo") {
		t.Error("an unwired repo should be flagged")
	}
	if !strings.Contains(body, "no .mcp.json") {
		t.Error("the badge should say what is missing")
	}
}
