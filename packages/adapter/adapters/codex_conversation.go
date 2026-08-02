package adapters

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/gmuxapp/gmux/packages/adapter"
)

var (
	_ adapter.ConversationRenderer         = (*Codex)(nil)
	_ adapter.ConversationExchangeRenderer = (*Codex)(nil)
)

// RenderConversation projects Codex's persisted rollout into visible user and
// assistant messages. Canonical response_item messages take precedence over
// legacy persisted deltas, which are used only when no completed message exists.
func (c *Codex) RenderConversation(ref string) ([]adapter.ConversationMessage, error) {
	rollout, err := readCodexRollout(ref)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.ConversationMessage, 0, len(rollout.messages))
	for _, item := range rollout.messages {
		if item.Text == "" {
			continue
		}
		out = append(out, adapter.ConversationMessage{Role: item.Role, Text: item.Text, Prose: item.Prose})
	}
	return out, nil
}

// RenderConversationExchanges reconstructs user-bounded exchanges. Current
// Codex rollouts persist a non-null token_count event once for every completed
// model response, after all of that response's message and tool-call items.
// That marker—not an individual message or function call—is the iteration
// boundary. This matters when one response mixes commentary and tools, and
// when a tool-only final response must clear prose from an earlier response.
func (c *Codex) RenderConversationExchanges(ref string) ([]adapter.Exchange, error) {
	rollout, err := readCodexRollout(ref)
	if err != nil {
		return nil, err
	}
	var out []adapter.Exchange
	for _, item := range rollout.exchanges {
		switch item.Role {
		case "user":
			if item.Text == "" { // injected context is not an exchange boundary
				continue
			}
			out = append(out, adapter.Exchange{Ordinal: uint64(len(out) + 1), User: item.Text})
		case "response":
			if len(out) == 0 {
				continue
			}
			ex := &out[len(out)-1]
			ex.Iterations++
			ex.Terminal = item.Text // deliberately replace, including with empty
		}
	}
	return out, nil
}

type codexMessage struct {
	Role  string
	Text  string
	Prose string
}

type codexRollout struct {
	messages  []codexMessage
	exchanges []codexMessage // roles are user and response
}

type codexRecord struct {
	Type    string `json:"type"`
	Payload struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Delta     string          `json:"delta"`
		Info      json.RawMessage `json:"info"`
	} `json:"payload"`
}

func readCodexRollout(ref string) (codexRollout, error) {
	data, err := os.ReadFile(ref)
	if err != nil {
		return codexRollout{}, err
	}
	var records []codexRecord
	hasResponseMarkers := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry codexRecord
		// Rollouts are append-only and may be observed during a partial final
		// write. Skip malformed and future record shapes without losing history.
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if isCodexResponseMarker(entry) {
			hasResponseMarkers = true
		}
		records = append(records, entry)
	}

	var out codexRollout
	var responseProse []string
	var pendingDelta strings.Builder
	for _, entry := range records {
		if entry.Type == "event_msg" && entry.Payload.Type == "agent_message_content_delta" {
			// Compatibility only: current Codex classifies these as transient and
			// does not persist them, but older rollouts may contain them.
			pendingDelta.WriteString(entry.Payload.Delta)
			continue
		}
		if isCodexResponseMarker(entry) {
			out.exchanges = append(out.exchanges, codexMessage{Role: "response", Text: strings.Join(responseProse, "\n\n")})
			responseProse = nil
			pendingDelta.Reset()
			continue
		}
		if entry.Type != "response_item" {
			continue
		}
		if entry.Payload.Type == "function_call" {
			out.messages = append(out.messages, codexMessage{
				Role: "assistant",
				Text: formatToolCall(entry.Payload.Name, codexToolArguments(entry.Payload.Arguments)),
			})
			continue
		}
		if entry.Payload.Type != "message" ||
			(entry.Payload.Role != "user" && entry.Payload.Role != "assistant") {
			continue
		}
		text, prose := codexMessageText(entry.Payload.Role, entry.Payload.Content)
		if entry.Payload.Role == "user" {
			// Never carry unfinished response prose across a new user boundary.
			responseProse = nil
			pendingDelta.Reset()
			out.messages = append(out.messages, codexMessage{Role: "user", Text: text, Prose: prose})
			out.exchanges = append(out.exchanges, codexMessage{Role: "user", Text: text})
			continue
		}

		pendingDelta.Reset() // canonical completed text supersedes legacy deltas
		out.messages = append(out.messages, codexMessage{Role: "assistant", Text: text, Prose: prose})
		if hasResponseMarkers {
			if prose != "" {
				responseProse = append(responseProse, prose)
			}
		} else {
			// Compatibility for older rollouts without response markers: their
			// persisted assistant messages are the best available boundary.
			out.exchanges = append(out.exchanges, codexMessage{Role: "response", Text: text})
		}
	}
	if pendingDelta.Len() != 0 {
		text := pendingDelta.String()
		out.messages = append(out.messages, codexMessage{Role: "assistant", Text: text, Prose: text})
		if !hasResponseMarkers {
			out.exchanges = append(out.exchanges, codexMessage{Role: "response", Text: text})
		}
	}
	return out, nil
}

func isCodexResponseMarker(entry codexRecord) bool {
	if entry.Type != "event_msg" || entry.Payload.Type != "token_count" {
		return false
	}
	info := strings.TrimSpace(string(entry.Payload.Info))
	return info != "" && info != "null"
}

func codexToolArguments(raw json.RawMessage) json.RawMessage {
	// Codex persists function-call arguments as a JSON string containing the
	// original JSON object, unlike pi and Claude which persist the object.
	var decoded string
	if json.Unmarshal(raw, &decoded) == nil {
		return json.RawMessage(decoded)
	}
	return raw
}

func codexMessageText(role string, raw json.RawMessage) (text, prose string) {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", ""
	}
	var parts, proseParts []string
	for _, block := range blocks {
		if role == "user" && block.Type == "input_image" {
			parts = append(parts, "[image]")
			continue
		}
		want := "output_text"
		if role == "user" {
			want = "input_text"
		}
		if block.Type != want || block.Text == "" || (role == "user" && isCodexSystemContext(block.Text)) {
			continue
		}
		parts = append(parts, block.Text)
		proseParts = append(proseParts, block.Text)
	}
	return strings.Join(parts, "\n\n"), strings.Join(proseParts, "\n\n")
}
