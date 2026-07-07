// Package acp defines gmux's internal normalized conversation schema — a
// deliberately minimal subset of the Agent Client Protocol (ACP), per
// ADR 0021. The runner produces these shapes (synthesized from the read-only
// pi extension for terminal pi, or re-published from a real ACP agent later);
// the frontend consumes them through one renderer regardless of adapter.
//
// SCOPE (tracer #1): only streaming assistant text. This package defines just
// the `session/load` history snapshot and the `session/update`
// `agent_message_chunk` variant. Thinking, tool calls, prompt/cancel, and the
// rest of ADR 0021 §7 are intentionally omitted; add them in later slices.
//
// CONVENTION (flagged for review — ADR 0021 tracer): the wire framing is
// JSON-RPC 2.0 objects over a WebSocket. Mirroring the PTY attach
// (ADR 0004 snapshot-then-stream), the server pushes an unsolicited
// `session/load` notification as the first frame (the history snapshot),
// then streams live `session/update` notifications. We do not implement a
// client→server `session/load` request for this slice; the snapshot is
// server-initiated exactly like the PTY renderScreen frame.
package acp

import "encoding/json"

// JSONRPCVersion is the fixed protocol tag on every frame.
const JSONRPCVersion = "2.0"

// Method names carried over the wire.
const (
	MethodSessionLoad   = "session/load"   // first frame: history snapshot
	MethodSessionUpdate = "session/update" // live token stream
)

// SessionUpdate discriminator values (ACP `update.sessionUpdate`).
const (
	UpdateAgentMessageChunk = "agent_message_chunk"
)

// ContentType discriminates a content block. Only text is used this slice.
const ContentTypeText = "text"

// Notification is a JSON-RPC 2.0 notification frame (no id, no response).
// Both the snapshot and the live stream are sent as notifications.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// ContentBlock is a single piece of message content. This slice emits only
// text blocks; Type is carried explicitly so later slices can add image /
// resource without a schema break.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// TextBlock is a convenience constructor for a text content block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: ContentTypeText, Text: text}
}

// Message is one turn in the conversation history snapshot: a role plus its
// content blocks. Roles mirror pi/ACP: "user" and "assistant" (this slice
// renders only these two; toolResult and others are dropped from the snapshot).
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// LoadParams is the params of the first `session/load` frame: the full
// conversation history known at connect time (durable JSONL + the in-memory
// unwritten tail).
type LoadParams struct {
	SessionID string    `json:"sessionId"`
	Messages  []Message `json:"messages"`
}

// UpdateParams is the params of a live `session/update` frame.
type UpdateParams struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

// SessionUpdate is the discriminated update payload. This slice emits only
// agent_message_chunk (SessionUpdate == UpdateAgentMessageChunk), carrying a
// single token-level text delta in Content.
type SessionUpdate struct {
	SessionUpdate string       `json:"sessionUpdate"`
	MessageID     string       `json:"messageId,omitempty"`
	Content       ContentBlock `json:"content"`
}

// NewLoad builds the snapshot notification frame.
func NewLoad(sessionID string, messages []Message) (Notification, error) {
	p, err := json.Marshal(LoadParams{SessionID: sessionID, Messages: messages})
	if err != nil {
		return Notification{}, err
	}
	return Notification{JSONRPC: JSONRPCVersion, Method: MethodSessionLoad, Params: p}, nil
}

// NewAgentMessageChunk builds a live token-delta notification frame.
func NewAgentMessageChunk(sessionID, messageID, delta string) (Notification, error) {
	p, err := json.Marshal(UpdateParams{
		SessionID: sessionID,
		Update: SessionUpdate{
			SessionUpdate: UpdateAgentMessageChunk,
			MessageID:     messageID,
			Content:       TextBlock(delta),
		},
	})
	if err != nil {
		return Notification{}, err
	}
	return Notification{JSONRPC: JSONRPCVersion, Method: MethodSessionUpdate, Params: p}, nil
}
