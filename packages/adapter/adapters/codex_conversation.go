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
// assistant messages. response_item is the canonical transcript record;
// event_msg records are intentionally ignored because they duplicate visible
// messages and also contain private reasoning, tool output, and progress data.
func (c *Codex) RenderConversation(ref string) ([]adapter.ConversationMessage, error) {
	items, err := readCodexMessages(ref)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.ConversationMessage, 0, len(items))
	for _, item := range items {
		text := codexMessageText(item.Role, item.Content)
		if text == "" {
			continue
		}
		out = append(out, adapter.ConversationMessage{Role: item.Role, Text: text, Prose: text})
	}
	return out, nil
}

// RenderConversationExchanges reconstructs user-bounded exchanges. Each
// persisted assistant message is one completed model iteration. Only the last
// assistant message's visible output_text is terminal prose; tool calls,
// reasoning, approvals, plans, compaction records, and tool results never leak.
func (c *Codex) RenderConversationExchanges(ref string) ([]adapter.Exchange, error) {
	items, err := readCodexMessages(ref)
	if err != nil {
		return nil, err
	}
	var out []adapter.Exchange
	for _, item := range items {
		switch item.Role {
		case "user":
			text := codexMessageText("user", item.Content)
			if text == "" { // injected context is not an exchange boundary
				continue
			}
			out = append(out, adapter.Exchange{Ordinal: uint64(len(out) + 1), User: text})
		case "assistant":
			if len(out) == 0 {
				continue
			}
			ex := &out[len(out)-1]
			ex.Iterations++
			ex.Terminal = codexMessageText("assistant", item.Content)
		}
	}
	return out, nil
}

type codexMessage struct {
	Role    string
	Content json.RawMessage
}

func readCodexMessages(ref string) ([]codexMessage, error) {
	data, err := os.ReadFile(ref)
	if err != nil {
		return nil, err
	}
	var out []codexMessage
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string          `json:"type"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"payload"`
		}
		// The file is append-only and may be observed during a partial final
		// write. Skip malformed and future record shapes without losing history.
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Type != "response_item" || entry.Payload.Type != "message" {
			continue
		}
		if entry.Payload.Role == "user" || entry.Payload.Role == "assistant" {
			out = append(out, codexMessage{Role: entry.Payload.Role, Content: entry.Payload.Content})
		}
	}
	return out, nil
}

func codexMessageText(role string, raw json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		want := "output_text"
		if role == "user" {
			want = "input_text"
		}
		if block.Type != want || block.Text == "" || (role == "user" && isCodexSystemContext(block.Text)) {
			continue
		}
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n\n")
}
