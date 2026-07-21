package distilator

import "context"

// ClaudeProvider is the default DistilatorProvider. It sends the current memory
// store plus a batch of transcripts to Claude and returns proposed changes with
// evidence and prevalence.
//
// Not yet implemented — build sequence step 5 (docs/architecture.md). When it
// is, model selection and the Anthropic client wiring live here.
type ClaudeProvider struct {
	// Anthropic client, model id, and prompt config land here.
}

var _ DistilatorProvider = (*ClaudeProvider)(nil)

func (c *ClaudeProvider) ProposeChanges(ctx context.Context, currentStore map[string][]byte, transcripts []Transcript, instructions string) ([]ProposedChange, error) {
	return nil, errNotImplemented
}
