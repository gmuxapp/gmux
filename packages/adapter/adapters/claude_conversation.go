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
	Type             string `json:"type"`
	UUID             string `json:"uuid"`
	ParentUUID       string `json:"parentUuid"`
	IsMeta           bool   `json:"isMeta"`
	IsSidechain      bool   `json:"isSidechain"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Message          *struct {
		ID      string          `json:"id"`
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

func (entry claudeConversationEntry) hidden() bool {
	return entry.IsMeta || entry.IsSidechain || entry.IsCompactSummary
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
	// Hidden records can be UUID-linked and trail the visible main
	// conversation while bookkeeping work is appended. They remain in byID
	// because a later main record may name one as an ancestor, but they must
	// never replace the latest eligible main-conversation leaf.
	leaf := claudeConversationEntry{}
	for i := len(linear) - 1; i >= 0; i-- {
		if linear[i].UUID != "" && !linear[i].hidden() {
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
			parts = append(parts, formatToolCall(block.Name, block.Input))
		case "image":
			parts = append(parts, "[image]")
			userBoundary = true
		}
	}
	return strings.Join(parts, "\n\n"), strings.Join(proseParts, "\n\n"), userBoundary
}

type claudeInjectedUserForm uint8

const (
	claudeNotInjected claudeInjectedUserForm = iota
	claudeCommandCaveat
	claudeCommand
	claudeCommandOutput
)

// claudeInjectedUserMatchers mirrors the flag-less local-command forms emitted
// by Claude Code. Claude Code is closed-source; these names come from the
// official 2.1.220 binary's slash/local-command tag vocabulary and were checked
// against persisted ~/.claude/projects transcript shapes. Content shape alone
// is intentionally insufficient: an exact XML wrapper can be a legitimate user
// prompt. Suppression additionally requires Claude's observed parent-linked
// caveat -> command -> output provenance chain.
var claudeInjectedUserMatchers = []struct {
	form  claudeInjectedUserForm
	match func(string) bool
}{
	{claudeCommandCaveat, claudeWrapped("local-command-caveat")}, // local command model caveat
	{claudeCommand, isClaudeCommandRecord},                       // command-name + command-message + command-args
	{claudeCommandOutput, claudeWrapped("local-command-stdout")}, // local command result renderer
	{claudeCommandOutput, claudeWrapped("local-command-stderr")}, // local command result renderer
}

func claudeInjectedForm(raw json.RawMessage) claudeInjectedUserForm {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return claudeNotInjected
	}
	text = strings.TrimSpace(text)
	for _, registered := range claudeInjectedUserMatchers {
		if registered.match(text) {
			return registered.form
		}
	}
	return claudeNotInjected
}

func isClaudeInjectedUserRecord(entries []claudeConversationEntry, index int) bool {
	if index < 0 || index >= len(entries) || entries[index].Message == nil {
		return false
	}
	switch claudeInjectedForm(entries[index].Message.Content) {
	case claudeCommand:
		return isProvenClaudeCommand(entries, index)
	case claudeCommandOutput:
		// Multiple stdout/stderr records may form one linked output chain. Walk
		// through only registered output forms until reaching its command.
		for index > 0 {
			previous := index - 1
			if entries[previous].Message == nil || !claudeEntriesLinked(entries[index], entries[previous]) {
				return false
			}
			switch claudeInjectedForm(entries[previous].Message.Content) {
			case claudeCommand:
				return isProvenClaudeCommand(entries, previous)
			case claudeCommandOutput:
				index = previous
			default:
				return false
			}
		}
	}
	return false
}

func isProvenClaudeCommand(entries []claudeConversationEntry, index int) bool {
	if index <= 0 || entries[index].Message == nil ||
		claudeInjectedForm(entries[index].Message.Content) != claudeCommand {
		return false
	}
	caveat := entries[index-1]
	return caveat.IsMeta && caveat.Message != nil &&
		claudeInjectedForm(caveat.Message.Content) == claudeCommandCaveat &&
		claudeEntriesLinked(entries[index], caveat)
}

func claudeEntriesLinked(child, parent claudeConversationEntry) bool {
	return child.ParentUUID != "" && parent.UUID != "" && child.ParentUUID == parent.UUID
}

func claudeWrapped(tag string) func(string) bool {
	return func(s string) bool {
		return strings.HasPrefix(s, "<"+tag+">") && strings.HasSuffix(s, "</"+tag+">")
	}
}

func isClaudeCommandRecord(s string) bool {
	for _, tag := range []string{"command-name", "command-message", "command-args"} {
		prefix, suffix := "<"+tag+">", "</"+tag+">"
		if !strings.HasPrefix(s, prefix) {
			return false
		}
		end := strings.Index(s[len(prefix):], suffix)
		if end < 0 {
			return false
		}
		s = strings.TrimSpace(s[len(prefix)+end+len(suffix):])
	}
	return s == ""
}

type claudeResponse struct {
	text  []string
	prose []string
}

type claudeResponseGroup struct {
	responses []claudeResponse
	byID      map[string]int
}

func (g *claudeResponseGroup) add(id, text, prose string) {
	index := -1
	if id != "" {
		if g.byID == nil {
			g.byID = make(map[string]int)
		}
		if found, ok := g.byID[id]; ok {
			index = found
		} else {
			index = len(g.responses)
			g.byID[id] = index
		}
	}
	// Legacy combined records have no message.id and each represent one
	// completed response. Current split records share an id across blocks.
	if index < 0 {
		index = len(g.responses)
	}
	if index == len(g.responses) {
		g.responses = append(g.responses, claudeResponse{})
	}
	if text != "" {
		g.responses[index].text = append(g.responses[index].text, text)
	}
	if prose != "" {
		g.responses[index].prose = append(g.responses[index].prose, prose)
	}
}

func (g *claudeResponseGroup) reset() {
	g.responses = nil
	g.byID = nil
}

// projectClaudeConversation groups the current one-record-per-content-block
// format by message.id while retaining compatibility with legacy combined
// records. Grouping is scoped to a real user boundary; hidden/tool-result
// records may interleave split blocks without creating or ending a response.
func projectClaudeConversation(entries []claudeConversationEntry) ([]adapter.ConversationMessage, []adapter.Exchange) {
	var messages []adapter.ConversationMessage
	var exchanges []adapter.Exchange
	var responses claudeResponseGroup

	flushResponses := func() {
		for _, response := range responses.responses {
			text := strings.Join(response.text, "\n\n")
			prose := strings.Join(response.prose, "\n\n")
			if text != "" {
				messages = append(messages, adapter.ConversationMessage{Role: "assistant", Text: text, Prose: prose})
			}
			if len(exchanges) != 0 {
				ex := &exchanges[len(exchanges)-1]
				ex.Iterations++
				ex.Terminal = prose // deliberately replace, including with empty
			}
		}
		responses.reset()
	}

	for index, entry := range entries {
		if entry.hidden() || entry.Message == nil || (entry.Type != "user" && entry.Type != "assistant") {
			continue
		}
		text, prose, boundary := renderClaudeContent(entry.Message.Content)
		switch entry.Type {
		case "user":
			if !boundary || isClaudeInjectedUserRecord(entries, index) {
				continue
			}
			flushResponses()
			messages = append(messages, adapter.ConversationMessage{Role: entry.Message.Role, Text: text, Prose: prose})
			exchanges = append(exchanges, adapter.Exchange{Ordinal: uint64(len(exchanges) + 1), User: text})
		case "assistant":
			responses.add(entry.Message.ID, text, prose)
		}
	}
	flushResponses()
	return messages, exchanges
}

func (c *Claude) RenderConversation(ref string) ([]adapter.ConversationMessage, error) {
	entries, err := readClaudeEntries(ref)
	if err != nil {
		return nil, err
	}
	messages, _ := projectClaudeConversation(entries)
	return messages, nil
}

func (c *Claude) RenderConversationExchanges(ref string) ([]adapter.Exchange, error) {
	entries, err := readClaudeEntries(ref)
	if err != nil {
		return nil, err
	}
	_, exchanges := projectClaudeConversation(entries)
	return exchanges, nil
}
