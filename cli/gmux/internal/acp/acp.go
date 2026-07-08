// Package acp defines gmux's internal normalized conversation schema — a
// deliberately minimal subset of the Agent Client Protocol (ACP), per
// ADR 0021. The runner produces these shapes (synthesized from the read-only
// pi extension for terminal pi, or re-published from a real ACP agent later);
// the frontend consumes them through one renderer regardless of adapter.
//
// SCOPE (slice #3): streaming assistant text, thinking, AND tool calls. This
// package defines the `session/load` history snapshot and the `session/update`
// `agent_message_chunk` (assistant text), `agent_thought_chunk` (reasoning),
// `tool_call` (a tool invocation) + `tool_call_update` (its status/output)
// variants. prompt/cancel and the rest of ADR 0021 §7 are intentionally
// omitted; add them in later slices.
//
// The full wire contract (both channels) is documented in
// docs/acp-conversation-stream.md.
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
	UpdateAgentMessageChunk = "agent_message_chunk" // assistant text delta
	UpdateAgentThoughtChunk = "agent_thought_chunk" // assistant reasoning/thinking delta
	UpdateToolCall          = "tool_call"           // a tool call appears (name + args)
	UpdateToolCallUpdate    = "tool_call_update"    // a tool call's status/output changes
)

// ContentType discriminates a content block.
const (
	ContentTypeText     = "text"      // assistant/user visible text
	ContentTypeThinking = "thinking"  // assistant reasoning (rendered distinctly)
	ContentTypeToolCall = "tool_call" // a tool invocation + its status/output
)

// Tool-call status values (mirroring ACP `toolCall.status`).
const (
	ToolStatusInProgress = "in_progress" // executing
	ToolStatusCompleted  = "completed"   // finished successfully
	ToolStatusFailed     = "failed"      // finished with an error
)

// Tool-call kind values (mirroring ACP `ToolKind`). The kind is the *semantic
// category* of a tool, which the frontend switches on to pick an icon, header
// shape, and body renderer — rather than matching the free-form tool name
// (ADR 0021 favors ACP wire names; ACP: "Tool kinds help Clients choose
// appropriate icons and optimize how they display tool execution progress").
const (
	ToolKindRead    = "read"    // reading files or data
	ToolKindEdit    = "edit"    // modifying/creating files or content
	ToolKindDelete  = "delete"  // removing files or data
	ToolKindMove    = "move"    // moving or renaming files
	ToolKindSearch  = "search"  // searching for information
	ToolKindExecute = "execute" // running commands or code
	ToolKindThink   = "think"   // internal reasoning or planning
	ToolKindFetch   = "fetch"   // retrieving external data
	ToolKindOther   = "other"   // anything else (default)
)

// KindForToolName maps a pi tool name to an ACP ToolKind. It is the single
// source of truth for the name→kind translation on the Go side (the durable
// JSONL history path); the pi extension mirrors this table for the live path
// (both are pi facts, translated at the typed-access point per ADR 0015).
// Unknown tools fall back to ToolKindOther, so rendering degrades gracefully.
func KindForToolName(name string) string {
	switch name {
	case "bash":
		return ToolKindExecute
	case "read", "ls":
		return ToolKindRead
	case "edit", "write":
		return ToolKindEdit
	case "grep", "find", "glob":
		return ToolKindSearch
	default:
		return ToolKindOther
	}
}

// Notification is a JSON-RPC 2.0 notification frame (no id, no response).
// Both the snapshot and the live stream are sent as notifications.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// ContentBlock is a single piece of message content. Type discriminates text
// vs thinking (and, in later slices, image / resource) without a schema break.
// Both text and thinking carry their payload in Text: the wire shape stays
// uniform, and Type tells the renderer how to present it.
//
// A tool-call block (Type == ContentTypeToolCall) is not text: it carries the
// invocation id, tool name, raw JSON arguments, an execution status, and (once
// finished) the textual output. Unlike text/thinking, a tool-call block is
// mutated in place by a later tool_call_update (status/output), keyed on
// ToolCallID, rather than appended to.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Tool-call fields (Type == ContentTypeToolCall).
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	Kind       string `json:"kind,omitempty"`   // ACP ToolKind (read/edit/execute/...)
	Args       string `json:"args,omitempty"`   // raw JSON arguments text
	Status     string `json:"status,omitempty"` // in_progress | completed | failed
	Output     string `json:"output,omitempty"` // textual tool result
}

// TextBlock is a convenience constructor for a text content block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: ContentTypeText, Text: text}
}

// ThinkingBlock is a convenience constructor for a reasoning content block.
func ThinkingBlock(text string) ContentBlock {
	return ContentBlock{Type: ContentTypeThinking, Text: text}
}

// ToolCallBlock is a convenience constructor for a tool-call content block in
// its initial (in-progress) state: id, name, kind, and raw JSON arguments.
// The kind is the caller's declared ACP ToolKind; an empty kind is left empty
// (the frontend treats a missing kind as "other").
func ToolCallBlock(id, name, kind, args string) ContentBlock {
	return ContentBlock{
		Type:       ContentTypeToolCall,
		ToolCallID: id,
		ToolName:   name,
		Kind:       kind,
		Args:       args,
		Status:     ToolStatusInProgress,
	}
}

// Message is one turn in the conversation history snapshot: a role plus its
// content blocks. Roles mirror pi/ACP: "user" and "assistant" (this slice
// renders only these two; toolResult and others are dropped from the snapshot).
//
// MessageID is set only for the in-flight assistant tail in a session/load
// snapshot: it carries the streaming id so a client that joins mid-turn keeps
// appending subsequent session/update deltas to the same message instead of
// starting a new one. Durable (JSONL) history messages leave it empty.
type Message struct {
	Role      string         `json:"role"`
	MessageID string         `json:"messageId,omitempty"`
	Content   []ContentBlock `json:"content"`
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

// SessionUpdate is the discriminated update payload. This slice emits
// agent_message_chunk (assistant text) and agent_thought_chunk (reasoning),
// each carrying a single token-level delta in Content.
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

// NewAgentMessageChunk builds a live assistant-text token-delta frame.
func NewAgentMessageChunk(sessionID, messageID, delta string) (Notification, error) {
	return newUpdate(sessionID, UpdateAgentMessageChunk, messageID, TextBlock(delta))
}

// NewAgentThoughtChunk builds a live reasoning/thinking token-delta frame,
// parallel to NewAgentMessageChunk but carrying a thinking content block.
func NewAgentThoughtChunk(sessionID, messageID, delta string) (Notification, error) {
	return newUpdate(sessionID, UpdateAgentThoughtChunk, messageID, ThinkingBlock(delta))
}

// NewToolCall builds a live frame announcing a new tool call (in progress). The
// content block carries the invocation id, tool name, and raw JSON arguments.
func NewToolCall(sessionID, messageID string, block ContentBlock) (Notification, error) {
	return newUpdate(sessionID, UpdateToolCall, messageID, block)
}

// NewToolCallUpdate builds a live frame mutating an existing tool call by id:
// the content block carries the same ToolCallID plus its new status and output.
func NewToolCallUpdate(sessionID, messageID string, block ContentBlock) (Notification, error) {
	return newUpdate(sessionID, UpdateToolCallUpdate, messageID, block)
}

func newUpdate(sessionID, kind, messageID string, content ContentBlock) (Notification, error) {
	p, err := json.Marshal(UpdateParams{
		SessionID: sessionID,
		Update: SessionUpdate{
			SessionUpdate: kind,
			MessageID:     messageID,
			Content:       content,
		},
	})
	if err != nil {
		return Notification{}, err
	}
	return Notification{JSONRPC: JSONRPCVersion, Method: MethodSessionUpdate, Params: p}, nil
}
