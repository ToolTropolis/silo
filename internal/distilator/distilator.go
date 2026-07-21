package distilator

import "context"

// Orchestrator runs a consolidation cycle: pull recent transcripts + the current
// store, ask the provider for proposed changes, and write them to a separate
// output path (_distilations/<run-id>/) — never touching the live store. It
// depends only on the provider interface, so the model backend is swappable.
//
// Not yet implemented — build sequence step 5 (docs/architecture.md).
type Orchestrator struct {
	provider DistilatorProvider
}

// NewOrchestrator wires an Orchestrator to a provider.
func NewOrchestrator(p DistilatorProvider) *Orchestrator {
	return &Orchestrator{provider: p}
}

// Run executes one consolidation cycle for a project. Proposals land in the
// output store for human review; promotion happens separately (review.go).
func (o *Orchestrator) Run(ctx context.Context, projectID string, sinceHours int) error {
	_ = ctx
	_ = projectID
	_ = sinceHours
	return errNotImplemented
}
