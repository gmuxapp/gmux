package adapters

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// Compile-time interface checks.
var (
	_ adapter.Launchable         = (*Codex)(nil)
	_ adapter.SessionFiler       = (*Codex)(nil)
	_ adapter.SessionFileLister  = (*Codex)(nil)
	_ adapter.FileMonitor        = (*Codex)(nil)
	_ adapter.FileAttributor     = (*Codex)(nil)
	_ adapter.Resumer            = (*Codex)(nil)
)

func init() {
	All = append(All, NewCodex())
}

// Codex is the adapter for OpenAI Codex CLI.
// Sessions are JSONL files in ~/.codex/sessions/YYYY/MM/DD/.
// Status is driven by event_msg lines in the session file (FileMonitor).
type Codex struct{}

func NewCodex() *Codex { return &Codex{} }

func (c *Codex) Name() string { return "codex" }

func (c *Codex) Discover() bool {
	_, err := exec.LookPath("codex")
	return err == nil
}

// Match returns true if any argument before "--" is the `codex` binary.
func (c *Codex) Match(cmd []string) bool {
	for _, arg := range cmd {
		if filepath.Base(arg) == "codex" {
			return true
		}
		if arg == "--" {
			break
		}
	}
	return false
}

// Env returns no extra environment variables.
func (c *Codex) Env(_ adapter.EnvContext) []string { return nil }

func (c *Codex) Launchers() []adapter.Launcher {
	return []adapter.Launcher{{
		ID:          "codex",
		Label:       "Codex",
		Command:     []string{"codex"},
		Description: "Coding Agent",
	}}
}

// Monitor is a no-op — status is driven by FileMonitor.ParseNewLines.
func (c *Codex) Monitor(_ []byte) *adapter.Status {
	return nil
}

// --- SessionFiler ---

// SessionRootDir returns Codex's sessions directory.
func (c *Codex) SessionRootDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

// SessionDir returns today's date-nested directory where Codex writes new
// session files. Codex organizes by date (YYYY/MM/DD), not by cwd.
// The scanner uses ListSessionFiles() for historical sessions across all dates.
func (c *Codex) SessionDir(_ string) string {
	root := c.SessionRootDir()
	if root == "" {
		return ""
	}
	now := time.Now()
	return filepath.Join(root, now.Format("2006"), now.Format("01"), now.Format("02"))
}

// ListSessionFiles walks the date-nested directory tree
// (~/.codex/sessions/YYYY/MM/DD/*.jsonl) and returns all session files.
func (c *Codex) ListSessionFiles() []string {
	root := c.SessionRootDir()
	if root == "" {
		return nil
	}
	var files []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable dirs
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	return files
}

// codexSessionMeta is the JSON shape of the session_meta payload.
type codexSessionMeta struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
}

// ParseSessionFile reads a Codex JSONL session file and returns display
// metadata.
// Title priority: first user prompt text > "(new)".
func (c *Codex) ParseSessionFile(path string) (*adapter.SessionFileInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		return nil, errEmpty
	}

	// Parse session_meta from first line.
	var firstLine struct {
		Type    string           `json:"type"`
		Payload codexSessionMeta `json:"payload"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &firstLine); err != nil {
		return nil, errNotSession
	}
	if firstLine.Type != "session_meta" {
		return nil, errNotSession
	}

	meta := firstLine.Payload
	created, _ := time.Parse(time.RFC3339Nano, meta.Timestamp)

	info := &adapter.SessionFileInfo{
		ID:       meta.ID,
		Cwd:      meta.Cwd,
		Created:  created,
		FilePath: path,
	}

	// Scan for user prompts and message count.
	var firstUserText string
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		if entry.Type != "response_item" {
			continue
		}

		switch {
		case entry.Payload.Role == "user" && entry.Payload.Type == "message":
			info.MessageCount++
			if firstUserText == "" {
				firstUserText = extractCodexUserText(entry.Payload.Content)
			}
		case entry.Payload.Role == "assistant" && entry.Payload.Type == "message":
			info.MessageCount++
		}
	}

	switch {
	case firstUserText != "":
		info.Title = truncateTitle(firstUserText, 80)
	default:
		info.Title = "(new)"
	}

	return info, nil
}

// --- FileMonitor ---

// ParseNewLines receives lines appended to an attributed session file
// and returns events for meaningful changes.
//
// Signals (from event_msg lines):
//   - user_message → working + title from preceding user response_item
//   - task_complete or turn_cancelled → idle
//
// Signals (from response_item lines):
//   - role:"user" type:"message" → extract text for title hint
func (c *Codex) ParseNewLines(lines []string, _ string) []adapter.FileEvent {
	var events []adapter.FileEvent

	for _, line := range lines {
		if line == "" {
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
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		switch entry.Type {
		case "event_msg":
			switch entry.Payload.Type {
			case "user_message":
				// User submitted a prompt — assistant will start working.
				// Title comes from ParseSessionFile on attribution, not here.
				events = append(events, adapter.FileEvent{
					Status: &adapter.Status{Working: true},
				})

			case "task_complete", "turn_cancelled", "turn_aborted":
				// Turn finished — clear status.
				events = append(events, adapter.FileEvent{
					Status: &adapter.Status{},
				})
			}
		}
	}
	return events
}

// --- FileAttributor ---

// AttributeFile matches a file to a session using the file's session_meta
// header (cwd + timestamp proximity). Codex uses date-nested directories
// shared by all sessions, so metadata matching is essential.
func (c *Codex) AttributeFile(filePath string, candidates []adapter.FileCandidate) string {
	info, err := c.ParseSessionFile(filePath)
	if err != nil {
		return ""
	}
	return attributeByMetadata(info, candidates)
}

// --- Resumer ---

// ResumeCommand returns the command to resume a Codex session.
func (c *Codex) ResumeCommand(info *adapter.SessionFileInfo) []string {
	return []string{"codex", "resume", info.ID}
}


// CanResume checks if a session file has user messages worth resuming.
func (c *Codex) CanResume(path string) bool {
	info, err := c.ParseSessionFile(path)
	if err != nil {
		return false
	}
	return info.MessageCount > 0
}

// --- Helpers ---

// extractCodexUserText extracts the first meaningful user text from a
// Codex response_item content array. Skips system-injected context blocks
// (environment_context, permissions, AGENTS.md instructions).
func extractCodexUserText(raw json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type != "input_text" || b.Text == "" {
			continue
		}
		// Skip system-injected context that isn't the user's actual prompt.
		if isCodexSystemContext(b.Text) {
			continue
		}
		return strings.TrimSpace(b.Text)
	}
	return ""
}

// isCodexSystemContext returns true if the text looks like Codex's
// injected system context rather than the user's actual prompt.
func isCodexSystemContext(s string) bool {
	// Codex injects several system blocks before the user's actual prompt.
	return strings.HasPrefix(s, "<permissions") ||
		strings.HasPrefix(s, "<environment_context>") ||
		strings.HasPrefix(s, "# AGENTS.md") ||
		strings.HasPrefix(s, "<turn_aborted>")
}
