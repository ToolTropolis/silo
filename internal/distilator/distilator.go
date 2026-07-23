package distilator

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// OutputPrefix is where a run's proposals are written — a separate path from
// the live memory store, so a Distilator run never modifies its own input.
const OutputPrefix = "_distilations"

// ProposalFile is the per-run manifest holding every proposed change.
const ProposalFile = "proposals.json"

// Store is the subset of the daemon's read/write surface the Distilator needs.
// Narrow by design: the orchestrator can read the live store and write to the
// output path, but the promote path (review.go) is what routes an approved
// change back through SafeWrite.
type Store interface {
	List(ctx context.Context, projectID, prefix string) ([]string, error)
	Read(ctx context.Context, projectID, path string) ([]byte, error)
	Write(ctx context.Context, projectID, path string, content []byte, actor, sessionID string) error
}

// TranscriptSource supplies the recent sessions a run consolidates from.
type TranscriptSource interface {
	Recent(ctx context.Context, projectID string, sinceHours int) ([]Transcript, error)
}

// Run is the record of one consolidation cycle.
type Run struct {
	RunID     string           `json:"run_id"`
	ProjectID string           `json:"project_id"`
	Proposals []ProposedChange `json:"proposals"`
}

// Orchestrator runs a consolidation cycle: pull recent transcripts + the current
// store, ask the provider for proposed changes, and write them to a separate
// output path (_distilations/<run-id>/) — never touching the live store. It
// depends only on the provider interface, so the model backend is swappable.
type Orchestrator struct {
	provider    DistilatorProvider
	store       Store
	transcripts TranscriptSource

	// MemoryPrefix bounds which paths count as the live memory store. Paths
	// under OutputPrefix are always excluded so a run never consolidates a
	// previous run's output.
	MemoryPrefix string
}

// NewOrchestrator wires an Orchestrator to a provider and its data sources.
func NewOrchestrator(p DistilatorProvider, s Store, t TranscriptSource) *Orchestrator {
	return &Orchestrator{provider: p, store: s, transcripts: t}
}

// Run executes one consolidation cycle for a project. Proposals land in the
// output store for human review; promotion happens separately (review.go).
//
// runID identifies this cycle and namespaces its output. The caller supplies it
// (rather than the orchestrator generating one) so runs are reproducible and
// the CLI can name them.
func (o *Orchestrator) Run(ctx context.Context, projectID, runID string, sinceHours int, instructions string) (*Run, error) {
	if projectID == "" || runID == "" {
		return nil, fmt.Errorf("distilator: projectID and runID are required")
	}
	if o.provider == nil || o.store == nil || o.transcripts == nil {
		return nil, fmt.Errorf("distilator: orchestrator is missing a provider, store, or transcript source")
	}

	current, err := o.loadLiveStore(ctx, projectID)
	if err != nil {
		return nil, err
	}

	sessions, err := o.transcripts.Recent(ctx, projectID, sinceHours)
	if err != nil {
		return nil, fmt.Errorf("distilator: load transcripts for %q: %w", projectID, err)
	}
	if len(sessions) == 0 {
		// Nothing to consolidate — an empty run is a success, not an error.
		return &Run{RunID: runID, ProjectID: projectID}, nil
	}

	proposals, err := o.provider.ProposeChanges(ctx, current, sessions, instructions)
	if err != nil {
		return nil, fmt.Errorf("distilator: propose changes for %q: %w", projectID, err)
	}

	// Validate what the provider returned before persisting it. A model can
	// return a well-formed-but-empty proposal object; without this, an
	// empty-path proposal is written to the manifest and only fails later, at
	// promote time, with a confusing "not proposed by run" error.
	kept := make([]ProposedChange, 0, len(proposals))
	for _, p := range proposals {
		if isReservedPath(p.Path) {
			return nil, fmt.Errorf("distilator: proposal targets a reserved namespace (%q); refusing", p.Path)
		}
		if strings.TrimSpace(p.Path) == "" {
			// Drop rather than fail the run: one malformed entry shouldn't
			// discard the other genuine proposals in the batch.
			continue
		}
		kept = append(kept, p)
	}
	proposals = kept

	run := &Run{RunID: runID, ProjectID: projectID, Proposals: proposals}
	if err := o.writeProposals(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

// loadLiveStore reads the project's current memory content, excluding anything
// under the output prefix so a run never treats prior proposals as live memory.
func (o *Orchestrator) loadLiveStore(ctx context.Context, projectID string) (map[string][]byte, error) {
	paths, err := o.store.List(ctx, projectID, o.MemoryPrefix)
	if err != nil {
		return nil, fmt.Errorf("distilator: list memory for %q: %w", projectID, err)
	}
	current := make(map[string][]byte, len(paths))
	for _, p := range paths {
		if isReservedPath(p) {
			continue
		}
		content, err := o.store.Read(ctx, projectID, p)
		if err != nil {
			return nil, fmt.Errorf("distilator: read %q: %w", p, err)
		}
		current[p] = content
	}
	return current, nil
}

// writeProposals persists a run's proposals to its own output path. This is the
// ONLY write the orchestrator performs, and it never touches the live store.
func (o *Orchestrator) writeProposals(ctx context.Context, run *Run) error {
	blob, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("distilator: encode proposals: %w", err)
	}
	out := RunPath(run.RunID, ProposalFile)
	if err := o.store.Write(ctx, run.ProjectID, out, blob, "distilator", run.RunID); err != nil {
		return fmt.Errorf("distilator: write proposals to %q: %w", out, err)
	}
	return nil
}

// RunPath returns a path inside a run's output namespace.
func RunPath(runID, name string) string {
	return path.Join(OutputPrefix, runID, name)
}

// isOutputPath reports whether a path lives in the Distilator output namespace.
func isOutputPath(p string) bool {
	return underPrefix(p, OutputPrefix)
}

// isReservedPath reports whether a path is in ANY namespace that is not live
// memory — run output or captured session transcripts. Both must be excluded
// from the live-store view, or a run would treat its own evidence (or a prior
// run's proposals) as memory to consolidate.
func isReservedPath(p string) bool {
	return underPrefix(p, OutputPrefix) || underPrefix(p, SessionPrefix)
}

func underPrefix(p, prefix string) bool {
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}
