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
var _ adapter.ConversationRenderer = (*Pi)(nil)

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
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.Type != "message" || entry.Message == nil {
			continue
		}
		role := entry.Message.Role
		if role != "user" && role != "assistant" {
			continue // toolResult and any future roles
		}
		text := renderPiContent(entry.Message.Content)
		if text == "" {
			continue // e.g. thinking-only assistant turn
		}
		out = append(out, adapter.ConversationMessage{Role: role, Text: text})
	}
	return out, nil
}

// renderPiContent renders a message's content to markdown. pi encodes
// content either as a plain string (old format) or as an array of
// typed blocks: text, thinking, toolCall, image.
func renderPiContent(content json.RawMessage) string {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return strings.TrimSpace(s)
	}

	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Name      string          `json:"name"`      // toolCall
		Arguments json.RawMessage `json:"arguments"` // toolCall
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}

	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		case "toolCall":
			parts = append(parts, formatPiToolCall(b.Name, b.Arguments))
		case "image":
			parts = append(parts, "[image]")
		}
		// thinking (and unknown block types): skipped.
	}
	return strings.Join(parts, "\n\n")
}

// maxToolArgChars caps the rendered tool-call arguments. Tool calls are
// activity context in the transcript, not payload: a bash command or an
// edit's full replacement text can run to kilobytes, which would drown
// the conversation the transcript exists to surface.
const maxToolArgChars = 120

// formatPiToolCall renders a tool call as a compact single line:
// "[tool] <name> <compact-json-args>". Plain text, no markdown
// emphasis or inline code — arguments are arbitrary bytes (shell
// commands full of backticks), so any markdown wrapping would break.
func formatPiToolCall(name string, args json.RawMessage) string {
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
