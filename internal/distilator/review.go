package distilator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tooltropolis/silo/internal/daemon"
)

// ErrRunNotFound is returned when a run's proposal manifest doesn't exist.
var ErrRunNotFound = errors.New("distilator: run not found")

// ErrProposalNotFound is returned when a run has no proposal for a path.
var ErrProposalNotFound = errors.New("distilator: proposal not found in run")

// SafeWriter is the daemon's CAS write path. Promotion goes through it (rather
// than a plain store write) so an approved change gets the same ETag/versioning
// treatment as any other write.
type SafeWriter interface {
	// The daemon.WriteOutcome return distinguishes a durable write from one
	// buffered locally because the backend was unreachable. Promotion treats
	// the latter as a failure: a human approved specific content, and calling
	// it promoted while it sits on local disk would misreport a reviewed change
	// as landed.
	SafeWrite(ctx context.Context, projectID, path string, edit func([]byte) []byte, actor, sessionID string) (daemon.WriteOutcome, error)
}

// Reviewer loads a run's proposals and promotes the approved ones.
type Reviewer struct {
	store  Store
	writer SafeWriter
}

// NewReviewer wires a Reviewer to the output store and the CAS write path.
func NewReviewer(s Store, w SafeWriter) *Reviewer {
	return &Reviewer{store: s, writer: w}
}

// ListRuns returns the run IDs a project has, newest first. Needed by any
// review surface (the dashboard, or `silo-distil`) to show what is pending
// without already knowing a run ID.
func (r *Reviewer) ListRuns(ctx context.Context, projectID string) ([]string, error) {
	paths, err := r.store.List(ctx, projectID, OutputPrefix+"/")
	if err != nil {
		return nil, fmt.Errorf("distilator: list runs for %q: %w", projectID, err)
	}
	seen := map[string]bool{}
	var runs []string
	for _, p := range paths {
		// paths look like _distilations/<run-id>/proposals.json
		rest := strings.TrimPrefix(p, OutputPrefix+"/")
		id, _, found := strings.Cut(rest, "/")
		if !found || id == "" || seen[id] {
			continue
		}
		seen[id] = true
		runs = append(runs, id)
	}
	// Run IDs are time-ordered (distil-<unix-nano>), so reverse-lexicographic
	// puts the newest first.
	sort.Sort(sort.Reverse(sort.StringSlice(runs)))
	return runs, nil
}

// LoadRun reads a run's proposal manifest from its output path.
func (r *Reviewer) LoadRun(ctx context.Context, projectID, runID string) (*Run, error) {
	blob, err := r.store.Read(ctx, projectID, RunPath(runID, ProposalFile))
	if err != nil {
		return nil, fmt.Errorf("distilator: load run %q: %w: %w", runID, ErrRunNotFound, err)
	}
	var run Run
	if err := json.Unmarshal(blob, &run); err != nil {
		return nil, fmt.Errorf("distilator: decode run %q: %w", runID, err)
	}
	return &run, nil
}

// Promote applies the approved proposals from a run to the live memory store,
// each through SafeWrite so it lands with the same CAS/versioning as a normal
// write. approvedPaths selects which of the run's proposals to apply — a
// human's decision, never inferred.
//
// Rejected proposals are simply not promoted; the run's output stays in place
// for audit (spec §6.7).
//
// Returns the paths actually promoted. On the first failure it stops and
// reports what had already been applied, so a partial promotion is visible
// rather than silent.
func (r *Reviewer) Promote(ctx context.Context, projectID, runID string, approvedPaths []string) ([]string, error) {
	if r.writer == nil {
		return nil, fmt.Errorf("distilator: no SafeWriter configured; cannot promote")
	}
	run, err := r.LoadRun(ctx, projectID, runID)
	if err != nil {
		return nil, err
	}

	byPath := make(map[string]ProposedChange, len(run.Proposals))
	for _, p := range run.Proposals {
		byPath[p.Path] = p
	}

	var promoted []string
	for _, want := range approvedPaths {
		proposal, ok := byPath[want]
		if !ok {
			return promoted, fmt.Errorf("distilator: %q not proposed by run %q: %w", want, runID, ErrProposalNotFound)
		}
		// Guard again at the promote boundary: never write into the output
		// namespace, even if a manifest were tampered with.
		if isOutputPath(proposal.Path) {
			return promoted, fmt.Errorf("distilator: refusing to promote into the output namespace (%q)", proposal.Path)
		}

		content := proposal.NewContent
		outcome, err := r.writer.SafeWrite(ctx, projectID, proposal.Path,
			func([]byte) []byte { return content },
			"distilator", promotedFrom(runID))
		if err != nil {
			return promoted, fmt.Errorf("distilator: promote %q from run %q: %w", proposal.Path, runID, err)
		}
		// Stop rather than report a promotion that only reached local disk. The
		// paths already returned did land, so the caller can see exactly how far
		// the promotion got.
		if outcome == daemon.WriteQueued {
			return promoted, fmt.Errorf(
				"distilator: promote %q from run %q: backend unreachable, the change was queued locally rather than promoted",
				proposal.Path, runID)
		}
		promoted = append(promoted, proposal.Path)
	}
	return promoted, nil
}

// promotedFrom is the session/tag value recorded on a promoted write so the
// audit trail shows which run a change came from (spec §6.6).
func promotedFrom(runID string) string { return "promoted_from:" + runID }
