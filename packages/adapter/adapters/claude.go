package adapters

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/filewatch"
	"github.com/gmuxapp/gmux/packages/paths"
)

// Compile-time interface checks.
var (
	_ adapter.ConversationSource    = (*Claude)(nil)
	_ adapter.ConversationProber    = (*Claude)(nil)
	_ adapter.Launchable            = (*Claude)(nil)
	_ adapter.ConversationDescriber = (*Claude)(nil)
	_ adapter.ConversationOpener    = (*Claude)(nil)
	_ adapter.Resumer               = (*Claude)(nil)
)

func init() {
	All = append(All, NewClaude())
}

// Claude is the adapter for Claude Code (claude CLI).
// Conversation files are JSONL in ~/.claude/projects/<encoded-cwd>/.
// Status is driven by the agent hook (see claude_hook.go), not PTY output.
type Claude struct{}

func NewClaude() *Claude { return &Claude{} }

func (c *Claude) Name() string { return "claude" }

func (c *Claude) Discover() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

// Match returns true if any argument before "--" is the `claude` binary.
func (c *Claude) Match(cmd []string) bool {
	for _, arg := range cmd {
		if filepath.Base(arg) == "claude" {
			return true
		}
		if arg == "--" {
			break
		}
	}
	return false
}

// Env returns no extra environment variables.
func (c *Claude) Env(_ adapter.EnvContext) []string { return nil }

func (c *Claude) Launchers() []adapter.Launcher {
	return []adapter.Launcher{{
		ID:          "claude",
		Label:       "Claude Code",
		Command:     []string{"claude"},
		Description: "Coding Agent",
	}}
}

// --- Conversation storage (file-backed: refs are absolute JSONL paths) ---

// ConversationRootDir returns Claude Code's per-project sessions directory.
func (c *Claude) ConversationRootDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// ConversationGone anchors deletion detection on ConversationRootDir
// (~/.claude/projects): if that tree is present, a missing transcript
// was deleted; if it's absent, the storage is unavailable. Refs are
// conversation-file paths for claude.
func (c *Claude) ConversationGone(ref string) (gone bool, ok bool) {
	return adapter.ConversationGoneAtRoot(ref, c.ConversationRootDir())
}

// encodeClaudeCwd encodes a working directory into Claude Code's directory
// naming: replace / and . with -.
// /home/mg/dev/gmux → -home-mg-dev-gmux
// /home/mg/.local/share/chezmoi → -home-mg--local-share-chezmoi
func encodeClaudeCwd(cwd string) string {
	return claudeCwdReplacer.Replace(paths.NormalizePath(cwd))
}

var claudeCwdReplacer = strings.NewReplacer("/", "-", ".", "-")

// ConversationDir returns Claude Code's session directory for a given cwd.
func (c *Claude) ConversationDir(cwd string) string {
	root := c.ConversationRootDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, encodeClaudeCwd(cwd))
}

// claudeFirstLine is the JSON shape of the first meaningful line in a
// Claude Code conversation file (type "user").
type claudeFirstLine struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// DescribeConversation reads a Claude Code JSONL conversation file (the ref
// is the absolute file path) and returns display metadata.
// Title priority: custom-title line > first user message text > "" (no
// conversation-derived title yet; callers fall back to cwd/adapter).
func (c *Claude) DescribeConversation(ref string) (*adapter.ConversationInfo, error) {
	path := ref
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		return nil, errEmpty
	}

	// A resumed Claude transcript has a new UUID filename, while replayed
	// history retains the original per-line sessionId. Keep every valid ID in
	// first-appearance order; malformed values must not create a false link.
	ownID := claudeConversationIDFromPath(path)
	lineSessionIDs := make([]string, 0)
	seenLineSessionIDs := make(map[string]struct{})

	// Find the first user line for session metadata.
	var firstUser *claudeFirstLine
	var customTitle string
	messageCount := 0

	for _, line := range lines {
		var session struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal([]byte(line), &session); err == nil {
			if id, ok := validClaudeConversationID(session.SessionID); ok && id != ownID {
				if _, exists := seenLineSessionIDs[id]; !exists {
					seenLineSessionIDs[id] = struct{}{}
					lineSessionIDs = append(lineSessionIDs, id)
				}
			}
		}

		if line == "" {
			continue
		}
		var peek struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &peek); err != nil {
			continue
		}

		switch peek.Type {
		case "user":
			messageCount++
			if firstUser == nil {
				var fl claudeFirstLine
				if err := json.Unmarshal([]byte(line), &fl); err == nil {
					firstUser = &fl
				}
			}
		case "assistant":
			messageCount++
		case "custom-title":
			var ct struct {
				CustomTitle string `json:"customTitle"`
			}
			if err := json.Unmarshal([]byte(line), &ct); err == nil && ct.CustomTitle != "" {
				customTitle = strings.TrimSpace(ct.CustomTitle)
			}
		}
	}

	if firstUser == nil {
		// No user messages — could be a queue-operation-only file.
		// Try to extract session ID from the first line.
		var header struct {
			SessionID string `json:"sessionId"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
			return nil, errNotSession
		}
		if header.SessionID == "" {
			return nil, errNotSession
		}

		created, _ := time.Parse(time.RFC3339Nano, header.Timestamp)
		return &adapter.ConversationInfo{
			ID:           header.SessionID,
			Title:        "", // header only, no messages yet
			Created:      created,
			LastActivity: fileLastActivity(path),
			MessageCount: messageCount,
			Ref:          path,
			AncestorIDs:  claudeAncestorIDs(nil, ownID, lineSessionIDs),
		}, nil
	}

	created, _ := time.Parse(time.RFC3339Nano, firstUser.Timestamp)

	info := &adapter.ConversationInfo{
		ID:           firstUser.SessionID,
		Cwd:          firstUser.Cwd,
		Created:      created,
		LastActivity: fileLastActivity(path),
		MessageCount: messageCount,
		Ref:          path,
	}

	firstUserText := extractClaudeUserText(firstUser.Message.Content)

	switch {
	case customTitle != "":
		info.Title = customTitle
	case firstUserText != "":
		info.Title = truncateTitle(firstUserText, 80)
	default:
		info.Title = "" // no custom title and no message yet
	}

	// Slug from the resolved title, custom-title included. Slug is the
	// session's mutable display name (UBIQUITOUS_LANGUAGE.md), not identity
	// — the immutable Tool ID is — so a /rename moves the slug with it,
	// matching pi and the hook path (claudeTitleSlug already slugs
	// session_title when the payload carries it).
	info.Slug = adapter.Slugify(info.Title)
	info.AncestorIDs = claudeAncestorIDs(firstUser.Message.Content, ownID, lineSessionIDs)

	return info, nil
}

// --- Resumer ---

// ResumeCommand returns the command to resume a Claude Code session.
func (c *Claude) ResumeCommand(info *adapter.ConversationInfo) []string {
	return []string{"claude", "--resume", info.ID}
}

// CanResume checks if a conversation has user messages worth resuming.
func (c *Claude) CanResume(ref string) bool {
	info, err := c.DescribeConversation(ref)
	if err != nil {
		return false
	}
	return info.MessageCount > 0
}

// OpenConversation streams the raw JSONL transcript at ref.
func (c *Claude) OpenConversation(ref string) (io.ReadCloser, error) {
	return os.Open(ref)
}

// --- Helpers ---

var (
	claudeUUIDPattern         = regexp.MustCompile(`(?i)^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$`)
	claudeSessionStartPattern = regexp.MustCompile(`(?i)# session-start -- ([0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12})\.jsonl`)
)

// claudeConversationIDFromPath returns the canonical UUID filename stem, if any.
func claudeConversationIDFromPath(path string) string {
	id, _ := validClaudeConversationID(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	return id
}

func validClaudeConversationID(id string) (string, bool) {
	if !claudeUUIDPattern.MatchString(id) {
		return "", false
	}
	return strings.ToLower(id), true
}

// claudeAncestorIDs derives resume lineage from the replayed per-line session
// IDs. The `# session-start -- <uuid>.jsonl` marker in the first user message
// is used ONLY to order the result (lead with it) — never as an independent
// source: that text is user-authorable, so trusting it alone would let a
// crafted first prompt forge an ancestor edge and drive a false takeover of an
// unrelated dead conversation (ADR 0024 §2 — a false link must be impossible;
// only a missed link is acceptable). Claude stamps every replayed line with
// the genuine ancestor's sessionId, so a real resume is always corroborated.
func claudeAncestorIDs(firstUserContent json.RawMessage, ownID string, lineSessionIDs []string) []string {
	ancestors := make([]string, 0, len(lineSessionIDs)+1)
	seen := make(map[string]struct{})
	add := func(id string) {
		if id == "" || id == ownID {
			return
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			ancestors = append(ancestors, id)
		}
	}

	// Line IDs are the trusted source. The marker only promotes a
	// corroborated ID to the front; an uncorroborated marker contributes
	// nothing.
	lineSet := make(map[string]struct{}, len(lineSessionIDs))
	for _, id := range lineSessionIDs {
		lineSet[id] = struct{}{}
	}
	content := extractClaudeUserText(firstUserContent)
	if match := claudeSessionStartPattern.FindStringSubmatch(content); len(match) == 2 {
		if id, ok := validClaudeConversationID(match[1]); ok {
			if _, corroborated := lineSet[id]; corroborated {
				add(id)
			}
		}
	}
	for _, id := range lineSessionIDs {
		add(id)
	}
	if len(ancestors) == 0 {
		return nil
	}
	return ancestors
}

// extractClaudeUserText extracts the first text block from a Claude Code
// user message content field. Content can be a string or array of blocks.
func extractClaudeUserText(raw json.RawMessage) string {
	// Try string content first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return cleanClaudeUserText(s)
	}

	// Try array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return cleanClaudeUserText(b.Text)
			}
		}
	}
	return ""
}

// contextRefPattern matches Claude Code's context reference blocks like
// <context ref="...">...</context> that appear inline in user messages.
var contextRefPattern = regexp.MustCompile(`<context\b[^>]*>[\s\S]*?</context>`)

// cleanClaudeUserText removes inline context references and leading
// @file references from user message text for title display.
func cleanClaudeUserText(s string) string {
	// Remove <context ...>...</context> blocks.
	s = contextRefPattern.ReplaceAllString(s, "")
	// Trim whitespace.
	s = strings.TrimSpace(s)
	// If what remains starts with @, it's likely just a file reference
	// with no actual prompt text — return empty.
	if s == "" {
		return ""
	}
	return s
}

// --- ConversationSource ---

func (c *Claude) SnapshotConversations(sink adapter.ConversationSink) {
	filewatch.Snapshot(c.ConversationRootDir(), ".jsonl", sink.Upsert)
}

func (c *Claude) WatchConversations(ctx context.Context, sink adapter.ConversationSink) error {
	return filewatch.Watch(ctx, c.ConversationRootDir(), ".jsonl", func(e filewatch.Event) {
		if e.Removed {
			sink.Remove(e.Path)
		} else {
			sink.Upsert(e.Path)
		}
	})
}
