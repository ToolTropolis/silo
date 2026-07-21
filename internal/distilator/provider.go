// Package distilator runs the out-of-band memory consolidation cycle: it reads
// a project's own sessions and its own memory store and proposes refinements,
// written to a separate output path and gated on human review before promotion.
// It never touches another project's data — it never has access to it.
package distilator

import (
	"context"
	"errors"
)

// Transcript is one captured session handed to the provider as evidence.
type Transcript struct {
	SessionID string
	Messages  []byte // serialized session content: messages + tool calls + metadata
}

// ProposedChange is a single diff the provider suggests for the memory store.
type ProposedChange struct {
	Path       string
	NewContent []byte
	Rationale  string
	Evidence   []string // session IDs that motivated this change
	Prevalence float64  // 0.0-1.0, how common the pattern was across the batch
}

// DistilatorProvider runs the actual "what should change in memory" judgment.
// The default implementation calls out to Claude (claude.go); the interface
// exists so a different model/provider can be swapped in without touching
// orchestration.
type DistilatorProvider interface {
	ProposeChanges(ctx context.Context, currentStore map[string][]byte, transcripts []Transcript, instructions string) ([]ProposedChange, error)
}

var errNotImplemented = errors.New("distilator: not implemented")
