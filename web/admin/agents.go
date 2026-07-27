package admin

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// agentDirs are the conventional places agent definitions live, relative to a
// repo root.
var agentDirs = []string{
	".claude/agents",
	"agents",
	".agents",
}

// Agent is one agent definition found in a repo.
type Agent struct {
	Name        string
	Description string
	Model       string
	File        string
	// Entries and Bytes are what this agent has written, when writes are
	// attributable. Zero means nothing yet — which is the useful signal: an
	// agent that never writes may not need Silo, or may not be reaching it.
	Entries   int
	Bytes     int64
	HasMemory bool
}

// AgentScan is what a repo yielded.
type AgentScan struct {
	Dir     string
	Agents  []Agent
	Problem string
}

// ScanAgents finds agent definitions in a repo.
//
// Reads only the frontmatter's name/description/model rather than parsing YAML:
// three known scalar fields do not justify a dependency, and a definition whose
// frontmatter is exotic enough to need a real parser is one this panel should
// skip rather than half-render.
func ScanAgents(repoPath string) AgentScan {
	var scan AgentScan
	if repoPath == "" {
		return scan
	}
	if !filepath.IsAbs(repoPath) {
		scan.Problem = "the recorded repository path is not absolute"
		return scan
	}

	dir := ""
	for _, candidate := range agentDirs {
		p := filepath.Join(repoPath, candidate)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			dir = p
			break
		}
	}
	if dir == "" {
		return scan // no agents here; not a problem, just nothing to show
	}
	scan.Dir = dir

	entries, err := os.ReadDir(dir)
	if err != nil {
		scan.Problem = err.Error()
		return scan
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		// Bounded read: a definition's frontmatter is at the top, and the body
		// can be arbitrarily long.
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		buf := make([]byte, 8192)
		n, _ := f.Read(buf)
		f.Close()

		a := parseAgentFrontmatter(string(buf[:n]))
		if a.Name == "" {
			a.Name = strings.TrimSuffix(e.Name(), ".md")
		}
		a.File = e.Name()
		scan.Agents = append(scan.Agents, a)
	}

	sort.Slice(scan.Agents, func(i, j int) bool { return scan.Agents[i].Name < scan.Agents[j].Name })
	return scan
}

// parseAgentFrontmatter pulls the three fields the panel shows out of a leading
// --- delimited block.
func parseAgentFrontmatter(content string) Agent {
	var a Agent
	if !strings.HasPrefix(content, "---") {
		return a
	}
	// Frontmatter runs to the next --- on its own line.
	rest := content[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return a
	}

	for _, line := range strings.Split(rest[:end], "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		// Only top-level keys: an indented line belongs to a nested structure
		// this parser deliberately does not understand.
		if key != strings.TrimLeft(key, " \t") {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)

		switch strings.TrimSpace(key) {
		case "name":
			a.Name = value
		case "description":
			a.Description = firstSentence(value)
		case "model":
			a.Model = value
		}
	}
	return a
}

// firstSentence trims a long description down to something a table cell can
// hold. Agent descriptions routinely embed whole worked examples.
func firstSentence(s string) string {
	if i := strings.Index(s, "\\n"); i > 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	const max = 160
	if len(s) > max {
		s = strings.TrimSpace(s[:max]) + "…"
	}
	return s
}

// hasMCPConfig reports whether a repo has a .mcp.json naming a silo server.
//
// Wiring is per-repo, not per-agent: one .mcp.json gives every agent in the
// repo the silo tools. So this is the honest answer to "is Silo configured for
// these agents?" — a per-agent flag would be inventing a distinction that does
// not exist.
func hasMCPConfig(repoPath string) bool {
	b, err := os.ReadFile(filepath.Join(repoPath, ".mcp.json"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), `"silo"`)
}
