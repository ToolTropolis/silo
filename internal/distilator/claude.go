package distilator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// defaultModel is the model the Distilator judges with. Opus for consolidation
// quality — this runs out-of-band, so latency matters far less than judgment.
const defaultModel = anthropic.ModelClaudeOpus4_8

// defaultMaxTokens bounds one proposal batch. Streaming isn't needed at this
// size, and the response is a single JSON manifest rather than long prose.
const defaultMaxTokens = 16000

// ClaudeProvider is the default DistilatorProvider. It sends the current memory
// store plus a batch of transcripts to Claude and returns proposed changes with
// evidence and prevalence.
//
// Authentication is subscription-based via an OAuth profile: run
// `ant auth login` once and the zero-argument client below picks the profile up
// automatically — no API key is stored in config or passed through the daemon.
// The SDK's credential chain also accepts ANTHROPIC_API_KEY or Workload
// Identity Federation if an operator prefers those.
type ClaudeProvider struct {
	client    anthropic.Client
	model     anthropic.Model
	maxTokens int64
}

var _ DistilatorProvider = (*ClaudeProvider)(nil)

// ClaudeOption customizes a ClaudeProvider.
type ClaudeOption func(*ClaudeProvider)

// WithModel overrides the model used for consolidation.
func WithModel(m anthropic.Model) ClaudeOption {
	return func(c *ClaudeProvider) { c.model = m }
}

// WithMaxTokens overrides the per-batch output budget.
func WithMaxTokens(n int64) ClaudeOption {
	return func(c *ClaudeProvider) { c.maxTokens = n }
}

// NewClaudeProvider builds the default provider. The zero-argument client
// resolves credentials from the standard chain (OAuth profile from
// `ant auth login`, ANTHROPIC_API_KEY, or WIF) — an unset API key does NOT mean
// unauthenticated. Credential problems surface on the first call, not here.
func NewClaudeProvider(opts ...ClaudeOption) *ClaudeProvider {
	p := &ClaudeProvider{
		client:    anthropic.NewClient(),
		model:     defaultModel,
		maxTokens: defaultMaxTokens,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// proposalSchema constrains the model's output to the ProposedChange shape, so
// orchestration gets validated JSON rather than prose it has to parse loosely.
var proposalSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"proposals"},
	"properties": map[string]any{
		"proposals": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"path", "new_content", "rationale", "evidence", "prevalence"},
				"properties": map[string]any{
					"path":        map[string]any{"type": "string"},
					"new_content": map[string]any{"type": "string"},
					"rationale":   map[string]any{"type": "string"},
					"evidence": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
					},
					"prevalence": map[string]any{"type": "number"},
				},
			},
		},
	},
}

const systemPrompt = `You consolidate an AI agent fleet's persistent memory.

You are given a project's CURRENT MEMORY STORE (markdown files) and a batch of
recent SESSION TRANSCRIPTS from that same project. Propose changes to the memory
store that would make future sessions more effective.

Rules:
- Propose a change only when the transcripts provide real evidence for it. Cite
  the session IDs that motivated each change in "evidence".
- "prevalence" is how common the pattern was across the batch, 0.0 to 1.0.
- "new_content" is the COMPLETE new content for that path, not a diff.
- Prefer updating an existing path over creating a near-duplicate one.
- Do not propose changes to paths beginning with "_distilations/".
- If the transcripts justify no changes, return an empty proposals array. An
  empty result is a valid and useful answer — do not invent changes.`

// ProposeChanges asks Claude what should change in memory, given the current
// store and a batch of transcripts.
func (c *ClaudeProvider) ProposeChanges(ctx context.Context, currentStore map[string][]byte, transcripts []Transcript, instructions string) ([]ProposedChange, error) {
	if len(transcripts) == 0 {
		return nil, nil // nothing to consolidate from
	}

	prompt, err := buildPrompt(currentStore, transcripts, instructions)
	if err != nil {
		return nil, err
	}

	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: proposalSchema},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("distilator: claude request failed (is a credential configured? run `ant auth login`): %w", err)
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return nil, fmt.Errorf("distilator: claude declined the consolidation request")
	}

	return parseProposals(resp)
}

// parseProposals pulls the structured manifest out of the response.
func parseProposals(resp *anthropic.Message) ([]ProposedChange, error) {
	var raw string
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			raw += t.Text
		}
	}
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("distilator: claude returned no proposal content")
	}

	var out struct {
		Proposals []struct {
			Path       string   `json:"path"`
			NewContent string   `json:"new_content"`
			Rationale  string   `json:"rationale"`
			Evidence   []string `json:"evidence"`
			Prevalence float64  `json:"prevalence"`
		} `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("distilator: decode proposals: %w", err)
	}

	changes := make([]ProposedChange, 0, len(out.Proposals))
	for _, p := range out.Proposals {
		changes = append(changes, ProposedChange{
			Path:       p.Path,
			NewContent: []byte(p.NewContent),
			Rationale:  p.Rationale,
			Evidence:   p.Evidence,
			Prevalence: p.Prevalence,
		})
	}
	return changes, nil
}

// buildPrompt renders the store and transcripts into the user turn.
func buildPrompt(currentStore map[string][]byte, transcripts []Transcript, instructions string) (string, error) {
	var b strings.Builder

	b.WriteString("# CURRENT MEMORY STORE\n\n")
	if len(currentStore) == 0 {
		b.WriteString("(empty — this project has no memory yet)\n\n")
	}
	for path, content := range currentStore {
		fmt.Fprintf(&b, "## %s\n\n```\n%s\n```\n\n", path, content)
	}

	b.WriteString("# SESSION TRANSCRIPTS\n\n")
	for _, t := range transcripts {
		fmt.Fprintf(&b, "## session %s\n\n```\n%s\n```\n\n", t.SessionID, t.Messages)
	}

	if strings.TrimSpace(instructions) != "" {
		fmt.Fprintf(&b, "# ADDITIONAL INSTRUCTIONS\n\n%s\n", instructions)
	}
	return b.String(), nil
}
