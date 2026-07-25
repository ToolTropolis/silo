package admin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// mcpConfigFile is the filename agent runtimes look for in a repo.
const mcpConfigFile = ".mcp.json"

// MCPServerEntry is one server's stanza in .mcp.json.
type MCPServerEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// MCPConfig is the whole file. Unknown top-level keys are preserved through a
// round trip so writing Silo's entry cannot silently drop a runtime's own
// settings.
type MCPConfig struct {
	Servers map[string]MCPServerEntry `json:"mcpServers"`
	// other holds any sibling keys the file had, so they survive a merge.
	other map[string]json.RawMessage
}

// SiloEntry builds the stanza for one project.
//
// The token is referenced as ${SILO_TOKEN} rather than embedded: .mcp.json is
// normally committed, and a token in it would be a live credential in git. The
// runtime expands it from the environment at launch.
func SiloEntry(projectID, daemonAddr, binary string) MCPServerEntry {
	if binary == "" {
		binary = "silo-mcp"
	}
	if daemonAddr == "" {
		daemonAddr = "http://127.0.0.1:8500"
	}
	return MCPServerEntry{
		Command: binary,
		Env: map[string]string{
			"SILO_TOKEN":       "${SILO_TOKEN}",
			"SILO_PROJECT":     projectID,
			"SILO_DAEMON_ADDR": daemonAddr,
		},
	}
}

// RenderMCPConfig returns the JSON for a fresh single-server config.
func RenderMCPConfig(projectID, daemonAddr, binary string) (string, error) {
	cfg := MCPConfig{Servers: map[string]MCPServerEntry{
		"silo": SiloEntry(projectID, daemonAddr, binary),
	}}
	return cfg.render()
}

func (c MCPConfig) render() (string, error) {
	// Marshal through a map so preserved sibling keys are emitted alongside
	// mcpServers rather than being dropped by a struct round trip.
	out := map[string]any{}
	for k, v := range c.other {
		out[k] = v
	}
	out["mcpServers"] = c.Servers

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("admin: render .mcp.json: %w", err)
	}
	return string(b) + "\n", nil
}

// MCPWritePlan describes what writing to a repo would do, so the operator sees
// it before anything happens.
type MCPWritePlan struct {
	Path string
	// Exists is true when the repo already has a .mcp.json.
	Exists bool
	// Conflict is true when it already defines a "silo" server, which a write
	// would replace.
	Conflict bool
	// OtherServers names servers that will be preserved, so a merge does not
	// look like it might discard them.
	OtherServers []string
	// Content is exactly what will be written.
	Content string
	// Warning is a non-blocking note, e.g. the directory not looking like a repo.
	Warning string
}

// PlanMCPWrite works out what writing Silo's entry into repoDir would do,
// without touching the filesystem.
//
// Separate from the write itself so the console can show a preview and get a
// confirmation. Writing into someone's repository is the one thing this surface
// does outside its own infrastructure, and it must never be a surprise.
func PlanMCPWrite(repoDir, projectID, daemonAddr, binary string) (MCPWritePlan, error) {
	var plan MCPWritePlan

	dir := strings.TrimSpace(repoDir)
	if dir == "" {
		return plan, fmt.Errorf("repository path is required")
	}
	if strings.HasPrefix(dir, "~") {
		// Expanding ~ here would guess at whose home directory is meant; the
		// console may not run as the user who owns the repo.
		return plan, fmt.Errorf("give an absolute path rather than one starting with ~")
	}
	if !filepath.IsAbs(dir) {
		return plan, fmt.Errorf("give an absolute path, e.g. /Users/you/code/myrepo")
	}
	dir = filepath.Clean(dir)

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return plan, fmt.Errorf("no such directory: %s", dir)
		}
		return plan, fmt.Errorf("cannot read %s: %w", dir, err)
	}
	if !info.IsDir() {
		return plan, fmt.Errorf("%s is a file, not a directory", dir)
	}

	plan.Path = filepath.Join(dir, mcpConfigFile)

	// A warning rather than an error: a worktree, a submodule, or a directory
	// about to become a repo are all legitimate, and refusing would be wrong.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		plan.Warning = "This does not look like a git repository (no .git). " +
			"Writing here is allowed, but check the path is what you meant."
	}

	cfg := MCPConfig{Servers: map[string]MCPServerEntry{}, other: map[string]json.RawMessage{}}

	existing, err := os.ReadFile(plan.Path)
	switch {
	case err == nil:
		plan.Exists = true
		if err := cfg.unmarshal(existing); err != nil {
			// Refuse rather than overwrite: a file we cannot parse may be
			// hand-edited config, and clobbering it would destroy work.
			return plan, fmt.Errorf("%s exists but is not valid JSON (%v). "+
				"Fix or move it first — refusing to overwrite it", plan.Path, err)
		}
		for name := range cfg.Servers {
			if name == "silo" {
				plan.Conflict = true
				continue
			}
			plan.OtherServers = append(plan.OtherServers, name)
		}
	case os.IsNotExist(err):
		// Fresh file.
	default:
		return plan, fmt.Errorf("cannot read %s: %w", plan.Path, err)
	}

	cfg.Servers["silo"] = SiloEntry(projectID, daemonAddr, binary)
	content, err := cfg.render()
	if err != nil {
		return plan, err
	}
	plan.Content = content
	return plan, nil
}

// WriteMCPConfig writes the planned content.
//
// Takes a plan rather than re-deriving it, so what lands on disk is exactly
// what the operator was shown and approved.
func WriteMCPConfig(plan MCPWritePlan) error {
	if plan.Path == "" || plan.Content == "" {
		return fmt.Errorf("nothing to write")
	}
	// Write via a temp file in the same directory, then rename: a crash or a
	// full disk mid-write must not leave a truncated .mcp.json in someone's
	// repo, which would break their agent rather than merely failing.
	dir := filepath.Dir(plan.Path)
	tmp, err := os.CreateTemp(dir, ".mcp.json.*")
	if err != nil {
		return fmt.Errorf("admin: write %s: %w", plan.Path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.WriteString(plan.Content); err != nil {
		tmp.Close()
		return fmt.Errorf("admin: write %s: %w", plan.Path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("admin: write %s: %w", plan.Path, err)
	}
	// 0644: .mcp.json holds no secret (the token is an env reference) and the
	// agent runtime needs to read it.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("admin: write %s: %w", plan.Path, err)
	}
	if err := os.Rename(tmpName, plan.Path); err != nil {
		return fmt.Errorf("admin: write %s: %w", plan.Path, err)
	}
	return nil
}

// unmarshal parses an existing .mcp.json, keeping sibling keys.
func (c *MCPConfig) unmarshal(data []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		if k == "mcpServers" {
			if err := json.Unmarshal(v, &c.Servers); err != nil {
				return fmt.Errorf("mcpServers: %w", err)
			}
			continue
		}
		c.other[k] = v
	}
	if c.Servers == nil {
		c.Servers = map[string]MCPServerEntry{}
	}
	return nil
}
