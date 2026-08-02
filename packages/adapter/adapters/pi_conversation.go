package adapters

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// Compile-time interface check (the main var block lives in pi.go; this
// one stays next to the implementation it guards).
var (
	_ adapter.ConversationRenderer         = (*Pi)(nil)
	_ adapter.ConversationExchangeRenderer = (*Pi)(nil)
)

// RenderConversation reconstructs a clean transcript from a pi JSONL
// conversation (the ref is the transcript's absolute path — pi's
// private ref convention, ADR 0022): user and assistant messages,
// oldest first, with tool calls rendered as compact one-liners.
//
// What is deliberately omitted:
//   - thinking blocks — internal reasoning, often huge, hidden by pi's
//     own transcript view too;
//   - toolResult messages — echoes of tool output (file reads, command
//     output) that dwarf the conversation itself;
//   - non-message entries (session header, model_change, compaction,
//     branch_summary, ...).
//
// A malformed line is skipped rather than failing the whole file: pi
// appends live, so the last line can be mid-write when we read it.
func (p *Pi) RenderConversation(ref string) ([]adapter.ConversationMessage, error) {
	data, err := os.ReadFile(ref)
	if err != nil {
		return nil, err
	}

	var out []adapter.ConversationMessage
	for _, entry := range piActiveBranch(data) {
		if entry.Type != "message" || entry.Message == nil {
			continue
		}
		role := entry.Message.Role
		if role != "user" && role != "assistant" {
			continue // toolResult and any future roles
		}
		text, prose := renderPiContent(entry.Message.Content)
		if text == "" {
			continue // e.g. thinking-only assistant turn
		}
		out = append(out, adapter.ConversationMessage{Role: role, Text: text, Prose: prose})
	}
	return out, nil
}

// RenderConversationExchanges projects pi's latest persisted branch into user
// bounded exchanges. Every assistant message counts, including thinking-only
// and tool-only responses; only prose from the final assistant message is the
// exchange's terminal response.
func (p *Pi) RenderConversationExchanges(ref string) ([]adapter.Exchange, error) {
	data, err := os.ReadFile(ref)
	if err != nil {
		return nil, err
	}
	var out []adapter.Exchange
	for _, entry := range piActiveBranch(data) {
		if entry.Type != "message" || entry.Message == nil {
			continue
		}
		switch entry.Message.Role {
		case "user":
			text, _ := renderPiExchangeContent(entry.Message.Content)
			out = append(out, adapter.Exchange{Ordinal: uint64(len(out) + 1), User: text})
		case "assistant":
			if len(out) == 0 {
				continue
			}
			_, prose := renderPiExchangeContent(entry.Message.Content)
			out[len(out)-1].Iterations++
			// Deliberately replace, including with empty: prose from an earlier
			// tool-use iteration is not the terminal response.
			out[len(out)-1].Terminal = prose
		}
	}
	return out, nil
}

type piConversationEntry struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	ParentID string `json:"parentId"`
	Message  *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// piActiveBranch applies pi's on-open leaf rule: the final persisted entry is
// the head and parentId links are walked to the root. Old/fixture transcripts
// without parent links remain linear.
func piActiveBranch(data []byte) []piConversationEntry {
	var linear []piConversationEntry
	hasParents := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry piConversationEntry
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry.ParentID != "" {
			hasParents = true
		}
		linear = append(linear, entry)
	}
	if !hasParents || len(linear) == 0 || linear[len(linear)-1].ID == "" {
		return linear
	}
	byID := make(map[string]piConversationEntry, len(linear))
	for _, entry := range linear {
		if entry.ID != "" {
			byID[entry.ID] = entry
		}
	}
	var reverse []piConversationEntry
	seen := map[string]bool{}
	for cur := linear[len(linear)-1]; cur.ID != "" && !seen[cur.ID]; {
		reverse = append(reverse, cur)
		seen[cur.ID] = true
		if cur.ParentID == "" {
			break
		}
		next, ok := byID[cur.ParentID]
		if !ok {
			break
		}
		cur = next
	}
	for left, right := 0, len(reverse)-1; left < right; left, right = left+1, right-1 {
		reverse[left], reverse[right] = reverse[right], reverse[left]
	}
	return reverse
}

// renderPiExchangeContent is the exchange-report variant: unlike the legacy
// transcript renderer it preserves source text whitespace exactly.
func renderPiExchangeContent(content json.RawMessage) (text, prose string) {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s, s
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return "", ""
	}
	var parts, proseParts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
				proseParts = append(proseParts, b.Text)
			}
		case "toolCall":
			parts = append(parts, formatToolCall(b.Name, b.Arguments))
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n\n"), strings.Join(proseParts, "\n\n")
}

// renderPiContent renders a message's content to markdown, returning the
// full rendering and its prose-only subset (see
// adapter.ConversationMessage.Prose). pi encodes content either as a
// plain string (old format) or as an array of typed blocks: text,
// thinking, toolCall, image.
//
// The two results are built in one pass because they differ only by
// which blocks contribute: a pi assistant message routinely mixes text
// blocks with toolCall blocks, so "the last assistant message" and "the
// last thing the agent said" are not the same string, and only the
// renderer knows which of its own output lines were tool renderings.
func renderPiContent(content json.RawMessage) (text, prose string) {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		s = strings.TrimSpace(s)
		// A whole-string content has no block types to separate: it is
		// prose by construction.
		return s, s
	}

	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Name      string          `json:"name"`      // toolCall
		Arguments json.RawMessage `json:"arguments"` // toolCall
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", ""
	}

	parts := make([]string, 0, len(blocks))
	proseParts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
				proseParts = append(proseParts, t)
			}
		case "toolCall":
			parts = append(parts, formatToolCall(b.Name, b.Arguments))
		case "image":
			parts = append(parts, "[image]")
		}
		// thinking (and unknown block types): skipped.
	}
	return strings.Join(parts, "\n\n"), strings.Join(proseParts, "\n\n")
}

// maxToolArgChars caps the rendered tool-call arguments. Tool calls are
// activity context in the transcript, not payload: a bash command or an
// edit's full replacement text can run to kilobytes, which would drown
// the conversation the transcript exists to surface.
const maxToolArgChars = 120

// formatToolCall renders a tool call as a compact single line:
// "[tool] <name> <compact-json-args>". Plain text, no markdown
// emphasis or inline code — arguments are arbitrary bytes (shell
// commands full of backticks), so any markdown wrapping would break.
func formatToolCall(name string, args json.RawMessage) string {
	if name == "" {
		name = "?"
	}
	var buf strings.Builder
	buf.WriteString("[tool] ")
	buf.WriteString(name)

	var dst bytes.Buffer
	if err := json.Compact(&dst, args); err != nil {
		return buf.String()
	}
	s := dst.String()
	if s == "{}" || s == "null" || s == "" {
		return buf.String()
	}
	if runes := []rune(s); len(runes) > maxToolArgChars {
		s = string(runes[:maxToolArgChars]) + "…"
	}
	buf.WriteString(" ")
	buf.WriteString(s)
	return buf.String()
}
