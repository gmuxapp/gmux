package adapters

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/gmuxapp/gmux/packages/adapter"
)

var (
	_ adapter.ConversationRenderer         = (*Claude)(nil)
	_ adapter.ConversationExchangeRenderer = (*Claude)(nil)
)

// claudeConversationEntry is the stable subset of Claude Code's append-only
// JSONL transcript. Tool results are encoded as user-role messages, so role
// alone is deliberately not enough to establish a user exchange boundary.
type claudeConversationEntry struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	ParentUUID  string `json:"parentUuid"`
	IsMeta      bool   `json:"isMeta"`
	IsSidechain bool   `json:"isSidechain"`
	Message     *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func readClaudeEntries(ref string) ([]claudeConversationEntry, error) {
	data, err := os.ReadFile(ref)
	if err != nil {
		return nil, err
	}
	var linear []claudeConversationEntry
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry claudeConversationEntry
		// A transcript can be observed while its final line is being appended.
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		linear = append(linear, entry)
	}
	return claudeActiveBranch(linear), nil
}

// claudeActiveBranch follows Claude's native parent-linked history from the
// final persisted node. Rewind/fork leaves abandoned messages in the JSONL;
// rendering file order would disclose and misattribute that abandoned branch.
// Older transcripts without parent links remain linear.
func claudeActiveBranch(linear []claudeConversationEntry) []claudeConversationEntry {
	if len(linear) == 0 {
		return nil
	}
	hasParents := false
	byID := make(map[string]claudeConversationEntry, len(linear))
	for _, entry := range linear {
		if entry.ParentUUID != "" {
			hasParents = true
		}
		if entry.UUID != "" {
			byID[entry.UUID] = entry
		}
	}
	if !hasParents {
		return linear
	}
	// Non-conversation bookkeeping records can trail the active message and
	// carry no UUID. They must not disable branch reconstruction.
	leaf := claudeConversationEntry{}
	for i := len(linear) - 1; i >= 0; i-- {
		if linear[i].UUID != "" {
			leaf = linear[i]
			break
		}
	}
	if leaf.UUID == "" {
		return linear
	}
	var reverse []claudeConversationEntry
	seen := make(map[string]bool)
	for cur := leaf; cur.UUID != "" && !seen[cur.UUID]; {
		reverse = append(reverse, cur)
		seen[cur.UUID] = true
		if cur.ParentUUID == "" {
			break
		}
		next, ok := byID[cur.ParentUUID]
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

// renderClaudeContent returns a visible rendering, its prose-only subset, and
// whether the content establishes a real user boundary. Thinking and tool
// results are never exposed; tool use is represented compactly without its
// result payload.
func renderClaudeContent(raw json.RawMessage) (text, prose string, userBoundary bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, s, s != ""
	}
	var blocks []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", "", false
	}
	var parts, proseParts []string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, block.Text)
				proseParts = append(proseParts, block.Text)
				userBoundary = true
			}
		case "tool_use":
			parts = append(parts, formatPiToolCall(block.Name, block.Input))
		case "image":
			parts = append(parts, "[image]")
			userBoundary = true
		}
	}
	return strings.Join(parts, "\n\n"), strings.Join(proseParts, "\n\n"), userBoundary
}

func (c *Claude) RenderConversation(ref string) ([]adapter.ConversationMessage, error) {
	entries, err := readClaudeEntries(ref)
	if err != nil {
		return nil, err
	}
	var out []adapter.ConversationMessage
	for _, entry := range entries {
		if entry.IsMeta || entry.IsSidechain || entry.Message == nil || (entry.Type != "user" && entry.Type != "assistant") {
			continue
		}
		text, prose, boundary := renderClaudeContent(entry.Message.Content)
		if entry.Type == "user" && !boundary { // tool_result message
			continue
		}
		if text != "" {
			out = append(out, adapter.ConversationMessage{Role: entry.Message.Role, Text: text, Prose: prose})
		}
	}
	return out, nil
}

func (c *Claude) RenderConversationExchanges(ref string) ([]adapter.Exchange, error) {
	entries, err := readClaudeEntries(ref)
	if err != nil {
		return nil, err
	}
	var out []adapter.Exchange
	for _, entry := range entries {
		if entry.IsMeta || entry.IsSidechain || entry.Message == nil {
			continue
		}
		text, prose, boundary := renderClaudeContent(entry.Message.Content)
		switch entry.Type {
		case "user":
			if boundary {
				out = append(out, adapter.Exchange{Ordinal: uint64(len(out) + 1), User: text})
			}
		case "assistant":
			if len(out) > 0 {
				out[len(out)-1].Iterations++
				// Only the latest assistant API iteration can be terminal prose.
				out[len(out)-1].Terminal = prose
			}
		}
	}
	return out, nil
}
