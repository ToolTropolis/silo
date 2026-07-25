package admin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoDir makes a temp directory that looks like a git repo.
func repoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	return dir
}

// The token must be an env reference, never a literal: .mcp.json is normally
// committed, so a token in it would be a live credential in git.
func TestRenderMCPConfig_TokenIsAnEnvReference(t *testing.T) {
	out, err := RenderMCPConfig("myrepo", "http://127.0.0.1:8500", "silo-mcp")
	if err != nil {
		t.Fatalf("RenderMCPConfig: %v", err)
	}
	if !strings.Contains(out, "${SILO_TOKEN}") {
		t.Error("the token must be referenced from the environment")
	}
	if strings.Contains(out, "silo_pat_") {
		t.Error("a literal token must never appear in the config")
	}
	if !strings.Contains(out, `"SILO_PROJECT": "myrepo"`) {
		t.Error("the config should be scoped to the project")
	}

	// It has to be valid JSON, or the agent runtime silently ignores it.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v", err)
	}
}

func TestPlanMCPWrite_NewFile(t *testing.T) {
	dir := repoDir(t)

	plan, err := PlanMCPWrite(dir, "myrepo", "http://127.0.0.1:8500", "silo-mcp")
	if err != nil {
		t.Fatalf("PlanMCPWrite: %v", err)
	}
	if plan.Exists {
		t.Error("a fresh repo has no .mcp.json")
	}
	if plan.Conflict {
		t.Error("there is nothing to conflict with")
	}
	if plan.Path != filepath.Join(dir, ".mcp.json") {
		t.Errorf("Path = %q", plan.Path)
	}
	if plan.Warning != "" {
		t.Errorf("a directory with .git should not warn: %q", plan.Warning)
	}
}

// The property that protects someone's existing setup: writing Silo's entry
// must not disturb another server's, or a sibling key the runtime relies on.
func TestPlanMCPWrite_PreservesOtherServersAndKeys(t *testing.T) {
	dir := repoDir(t)
	existing := `{
  "mcpServers": {
    "github": {"command": "gh-mcp", "args": ["--verbose"]},
    "postgres": {"command": "pg-mcp", "env": {"DSN": "${DSN}"}}
  },
  "someOtherKey": {"kept": true}
}`
	path := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan, err := PlanMCPWrite(dir, "myrepo", "http://127.0.0.1:8500", "silo-mcp")
	if err != nil {
		t.Fatalf("PlanMCPWrite: %v", err)
	}
	if !plan.Exists {
		t.Error("Exists should be true")
	}
	if plan.Conflict {
		t.Error("no existing silo entry, so no conflict")
	}
	if len(plan.OtherServers) != 2 {
		t.Errorf("OtherServers = %v, want both preserved servers named", plan.OtherServers)
	}

	if err := WriteMCPConfig(plan); err != nil {
		t.Fatalf("WriteMCPConfig: %v", err)
	}

	var got map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("written file is not valid JSON: %v", err)
	}
	servers, _ := got["mcpServers"].(map[string]any)
	for _, want := range []string{"github", "postgres", "silo"} {
		if _, ok := servers[want]; !ok {
			t.Errorf("server %q missing after the write", want)
		}
	}
	if _, ok := got["someOtherKey"]; !ok {
		t.Error("a sibling top-level key was dropped")
	}
	// The preserved entries must keep their own settings, not just their names.
	gh, _ := servers["github"].(map[string]any)
	if gh["command"] != "gh-mcp" {
		t.Errorf("github entry was altered: %v", gh)
	}
}

// Replacing an existing silo entry is flagged so the console can demand a
// second confirmation — the operator may be pointing at the wrong repo.
func TestPlanMCPWrite_DetectsSiloConflict(t *testing.T) {
	dir := repoDir(t)
	existing := `{"mcpServers": {"silo": {"command": "silo-mcp", "env": {"SILO_PROJECT": "otherproject"}}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(existing), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan, err := PlanMCPWrite(dir, "myrepo", "", "")
	if err != nil {
		t.Fatalf("PlanMCPWrite: %v", err)
	}
	if !plan.Conflict {
		t.Error("an existing silo entry must be flagged as a conflict")
	}
}

// Refusing to overwrite unparseable JSON: it may be hand-edited config, and
// clobbering it would destroy work.
func TestPlanMCPWrite_RefusesToClobberInvalidJSON(t *testing.T) {
	dir := repoDir(t)
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := PlanMCPWrite(dir, "myrepo", "", "")
	if err == nil {
		t.Fatal("an unparseable .mcp.json must not be silently overwritten")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error should say it is refusing, got %q", err)
	}
}

func TestPlanMCPWrite_RejectsBadPaths(t *testing.T) {
	tests := []struct {
		name, path, wantErr string
	}{
		{"empty", "", "required"},
		{"relative", "relative/path", "absolute"},
		{"tilde", "~/code/myrepo", "absolute"},
		{"missing", "/nonexistent/definitely/not/here", "no such directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanMCPWrite(tc.path, "myrepo", "", "")
			if err == nil {
				t.Fatalf("PlanMCPWrite(%q) should fail", tc.path)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestPlanMCPWrite_RejectsAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := PlanMCPWrite(file, "myrepo", "", ""); err == nil {
		t.Error("a file path must be rejected")
	}
}

// A non-repo directory warns rather than blocks: worktrees, submodules, and
// not-yet-initialised repos are all legitimate.
func TestPlanMCPWrite_WarnsOnNonRepo(t *testing.T) {
	dir := t.TempDir() // no .git

	plan, err := PlanMCPWrite(dir, "myrepo", "", "")
	if err != nil {
		t.Fatalf("a non-repo directory should be allowed: %v", err)
	}
	if plan.Warning == "" {
		t.Error("a directory with no .git should warn")
	}
}

func TestWriteMCPConfig_WritesReadableFile(t *testing.T) {
	dir := repoDir(t)
	plan, err := PlanMCPWrite(dir, "myrepo", "http://127.0.0.1:8500", "silo-mcp")
	if err != nil {
		t.Fatalf("PlanMCPWrite: %v", err)
	}
	if err := WriteMCPConfig(plan); err != nil {
		t.Fatalf("WriteMCPConfig: %v", err)
	}

	info, err := os.Stat(plan.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The agent runtime has to read it, and it holds no secret.
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %v, want 0644", perm)
	}

	raw, _ := os.ReadFile(plan.Path)
	if string(raw) != plan.Content {
		t.Error("what was written differs from what the plan previewed")
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mcp.json.") {
			t.Errorf("a temp file was left behind: %s", e.Name())
		}
	}
}

func TestWriteMCPConfig_RejectsEmptyPlan(t *testing.T) {
	if err := WriteMCPConfig(MCPWritePlan{}); err == nil {
		t.Error("an empty plan must not write anything")
	}
}

// Writing twice must be idempotent — an operator re-running the step should not
// end up with a different file.
func TestWriteMCPConfig_IsIdempotent(t *testing.T) {
	dir := repoDir(t)

	plan1, _ := PlanMCPWrite(dir, "myrepo", "http://127.0.0.1:8500", "silo-mcp")
	if err := WriteMCPConfig(plan1); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first, _ := os.ReadFile(plan1.Path)

	plan2, err := PlanMCPWrite(dir, "myrepo", "http://127.0.0.1:8500", "silo-mcp")
	if err != nil {
		t.Fatalf("re-plan: %v", err)
	}
	if !plan2.Conflict {
		t.Error("the second plan should see its own entry as a conflict")
	}
	if err := WriteMCPConfig(plan2); err != nil {
		t.Fatalf("second write: %v", err)
	}
	second, _ := os.ReadFile(plan2.Path)

	if string(first) != string(second) {
		t.Errorf("writing twice produced different files:\n%s\nvs\n%s", first, second)
	}
}
