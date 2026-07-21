// Package transcript captures session content (messages + tool calls +
// metadata) so the Distilator has evidence to consolidate from.
package transcript

// Capture is a serialized session, keyed by session ID. Not yet implemented —
// build sequence step 5 (docs/architecture.md).
type Capture struct {
	SessionID string
	Messages  []byte
}
