package adapters

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/paths"
)

// Compile-time interface checks.
var (
	_ adapter.Launchable      = (*Pi)(nil)
	_ adapter.SessionFiler    = (*Pi)(nil)
	_ adapter.FileMonitor     = (*Pi)(nil)
	_ adapter.FileAttributor  = (*Pi)(nil)
	_ adapter.Resumer         = (*Pi)(nil)
)

func init() {
	All = append(All, NewPi())
}

// Pi is the adapter for the pi coding agent.
// Status is driven by the JSONL session file (FileMonitor), not PTY output.
// Implements SessionFiler, FileMonitor, and Resumer.
type Pi struct{}

func NewPi() *Pi { return &Pi{} }

func (p *Pi) Name() string { return "pi" }

func (p *Pi) Discover() bool {
	// Fast path: check if 'pi' binary exists on PATH without executing it.
	// Running `pi --version` is too slow (~3s for Node.js startup).
	_, err := exec.LookPath("pi")
	return err == nil
}

// Match returns true if any argument in the command is the `pi` or
// `pi-coding-agent` binary.
func (p *Pi) Match(cmd []string) bool {
	for _, arg := range cmd {
		base := filepath.Base(arg)
		if base == "pi" || base == "pi-coding-agent" {
			return true
		}
		if arg == "--" {
			break
		}
	}
	return false
}

// Env returns no extra environment variables.
func (p *Pi) Env(_ adapter.EnvContext) []string { return nil }

func (p *Pi) Launchers() []adapter.Launcher {
	return []adapter.Launcher{{
		ID:          "pi",
		Label:       "pi",
		Command:     []string{"pi"},
		Description: "Coding agent",
	}}
}

// Monitor is a no-op for the pi adapter — status is driven by the
// JSONL session file via FileMonitor.ParseNewLines instead of PTY output.
// This avoids flicker from spinner redraws.
func (p *Pi) Monitor(_ []byte) *adapter.Status {
	return nil
}

// --- SessionFiler ---

// SessionRootDir returns pi's top-level sessions directory.
// Respects PI_CODING_AGENT_DIR (pi's own env var for overriding the
// agent data directory, default ~/.pi/agent). This lets dev instances
// use an isolated session store.
func (p *Pi) SessionRootDir() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}

// SessionDir returns pi's session directory for a given cwd.
// Pi encodes: strip leading /, replace remaining / with -, wrap in --.
// /home/mg/dev/gmux → --home-mg-dev-gmux--
func (p *Pi) SessionDir(cwd string) string {
	root := p.SessionRootDir()
	if root == "" {
		return ""
	}
	abs := paths.NormalizePath(cwd)
	path := strings.TrimPrefix(abs, "/")
	encoded := "--" + strings.ReplaceAll(path, "/", "-") + "--"
	return filepath.Join(root, encoded)
}

// ParseSessionFile reads a pi JSONL session file and returns display metadata.
// Title priority: session_info.name > first user message > "(new)".
func (p *Pi) ParseSessionFile(path string) (*adapter.SessionFileInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		return nil, errEmpty
	}

	var header struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Cwd       string `json:"cwd"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		return nil, err
	}
	if header.Type != "session" {
		return nil, errNotSession
	}

	created, _ := time.Parse(time.RFC3339Nano, header.Timestamp)

	info := &adapter.SessionFileInfo{
		ID:       header.ID,
		Cwd:      header.Cwd,
		Created:  created,
		FilePath: path,
	}

	var name string
	var firstUserMsg string

	for _, line := range lines[1:] {
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
		case "session_info":
			var si struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(line), &si); err == nil && si.Name != "" {
				name = strings.TrimSpace(si.Name)
			}
		case "message":
			info.MessageCount++
			if firstUserMsg == "" {
				firstUserMsg = extractFirstUserText(line)
			}
		}
	}

	switch {
	case name != "":
		info.Title = name
	case firstUserMsg != "":
		info.Title = truncateTitle(firstUserMsg, 80)
	default:
		info.Title = "(new)"
	}

	info.Slug = adapter.Slugify(info.Title)

	return info, nil
}

// --- FileMonitor ---

// ParseNewLines receives lines appended to an attributed session file
// and returns events for meaningful changes.
//
// Signals:
//   - session_info with name → title update (no status change)
//   - message role:"user" → working (assistant will respond)
//   - message role:"assistant" — status depends on stopReason:
//   - "toolUse" → working (tool loop continues)
//   - "stop"    → idle (turn complete)
//   - "aborted" → idle (user cancelled via Esc)
//   - "error"   → error state only if retries exhausted (consecutive errors
//     reach pi's retry limit); otherwise no change (retry expected)
//
// Unknown event types and unknown stopReasons produce no state change.
// Extensions can emit custom events; these must not disrupt existing state.
func (p *Pi) ParseNewLines(lines []string, filePath string) []adapter.FileEvent {
	var events []adapter.FileEvent
	for _, line := range lines {
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
		case "session_info":
			var si struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(line), &si); err == nil && si.Name != "" {
				events = append(events, adapter.FileEvent{
					Title: strings.TrimSpace(si.Name),
				})
			}

		case "message":
			var msg struct {
				Message *struct {
					Role       string `json:"role"`
					StopReason string `json:"stopReason"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.Message == nil {
				continue
			}

			switch msg.Message.Role {
			case "user":
				// User submitted a message — assistant will start working.
				events = append(events, adapter.FileEvent{
					Status: &adapter.Status{Working: true},
				})

			case "assistant":
				switch msg.Message.StopReason {
				case "toolUse":
					// Assistant wants to call tools — agent loop continues.
					events = append(events, adapter.FileEvent{
						Status: &adapter.Status{Working: true},
					})
				case "stop":
					// Assistant finished its turn — clear status, mark unread.
					events = append(events, adapter.FileEvent{
						Status: &adapter.Status{},
						Unread: adapter.BoolPtr(true),
					})
				case "aborted":
					// User pressed Esc to cancel — agent is idle.
					events = append(events, adapter.FileEvent{
						Status: &adapter.Status{},
					})
				case "error":
					// Errors are often transient (overloaded, rate-limited)
					// and pi retries automatically. While retries are
					// pending, don't change state (stay working). When
					// retries are exhausted, signal error so the frontend
					// can show a red dot.
					if filePath != "" {
						count, cwd := countTrailingErrors(filePath)
						if count >= piMaxRetries(cwd) {
							// Retries exhausted — agent gave up.
							events = append(events, adapter.FileEvent{
								Status: &adapter.Status{Error: true},
							})
						}
					}
				// Unknown stopReasons: no state change.
				}
			}
		}
	}
	return events
}

// piDefaultMaxRetries is the fallback when settings can't be read.
// Pi's default is maxRetries=3, so exhaustion = 1 original + 3 retries = 4.
const piDefaultMaxRetries = 4

// piMaxRetries reads pi's retry setting from its config files.
// Returns maxRetries+1 (the total number of error messages when exhausted).
// Pi merges ~/.pi/agent/settings.json (global) with <cwd>/.pi/settings.json
// (project-level); project settings take precedence.
func piMaxRetries(cwd string) int {
	read := func(path string) int {
		data, err := os.ReadFile(path)
		if err != nil {
			return -1
		}
		var cfg struct {
			Retry *struct {
				MaxRetries *int `json:"maxRetries"`
			} `json:"retry"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil || cfg.Retry == nil || cfg.Retry.MaxRetries == nil {
			return -1
		}
		return *cfg.Retry.MaxRetries
	}

	// Project-level overrides global (matches pi's deepMergeSettings).
	if cwd != "" {
		if v := read(filepath.Join(cwd, ".pi", "settings.json")); v >= 0 {
			return v + 1
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return piDefaultMaxRetries
	}
	if v := read(filepath.Join(home, ".pi", "agent", "settings.json")); v >= 0 {
		return v + 1
	}
	return piDefaultMaxRetries
}

// countTrailingErrors reads a pi session file and counts consecutive
// assistant error messages from the end, ignoring non-message lines
// (custom events, labels, etc.). Also extracts the cwd from the
// session header for config lookup.
func countTrailingErrors(filePath string) (count int, cwd string) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return 0, ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")

	// Extract cwd from the session header (first line).
	if len(lines) > 0 {
		var header struct {
			Type string `json:"type"`
			Cwd  string `json:"cwd"`
		}
		if err := json.Unmarshal([]byte(lines[0]), &header); err == nil && header.Type == "session" {
			cwd = header.Cwd
		}
	}

	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if line == "" {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message *struct {
				Role       string `json:"role"`
				StopReason string `json:"stopReason"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		// Skip non-message lines (custom events, labels, etc.)
		if entry.Type != "message" {
			continue
		}
		if entry.Message != nil && entry.Message.Role == "assistant" && entry.Message.StopReason == "error" {
			count++
		} else {
			break // hit a non-error message, stop counting
		}
	}
	return count, cwd
}

// --- Resumer ---

// ResumeCommand returns the command to resume a pi session.
func (p *Pi) ResumeCommand(info *adapter.SessionFileInfo) []string {
	return []string{"pi", "--session", info.FilePath, "-c"}
}

// CanResume checks if a session file is valid and has content worth resuming.
func (p *Pi) CanResume(path string) bool {
	info, err := p.ParseSessionFile(path)
	if err != nil {
		return false
	}
	return info.MessageCount > 0
}

// --- Helpers ---

// ListSessionFiles returns all .jsonl files in a directory.
func ListSessionFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files
}

// --- FileAttributor ---

// AttributeFile matches a session file to a live session using content
// similarity between the file's conversation text and each candidate's
// terminal scrollback.
//
// The file text is extracted from all message types (user, assistant,
// toolResult) and compared against each candidate's scrollback after
// stripping box-drawing characters, markdown formatting, and collapsing
// whitespace. These normalizations are needed because the scrollback is
// a terminal rendering of the same content the file stores as structured
// JSONL.
func (p *Pi) AttributeFile(filePath string, candidates []adapter.FileCandidate) string {
	fileText, err := extractPiText(filePath)
	if err != nil {
		return ""
	}
	return attributeByScrollbackNormalized(fileText, candidates)
}

// extractPiText reads the tail of a pi JSONL session file and extracts
// conversation text from all message types (user, assistant, toolResult).
// Including tool output is important because it dominates the scrollback.
//
// Only the last 32KB is read since we only need tail(200) of the
// extracted text. Session files can be tens of MB; reading them fully
// for every unattributed file in a directory would be costly on startup.
func extractPiText(path string) (string, error) {
	const maxRead = 32 * 1024

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	offset := info.Size() - maxRead
	if offset < 0 {
		offset = 0
	}
	if offset > 0 {
		f.Seek(offset, 0)
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}

	// If we seeked into the middle of a line, skip the partial first line.
	if offset > 0 {
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			data = data[idx+1:]
		}
	}

	var out strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message *struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Message == nil {
			continue
		}
		// Try array of content blocks (user/assistant messages).
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(entry.Message.Content, &blocks); err == nil {
			for _, b := range blocks {
				if b.Text != "" {
					out.WriteString(b.Text)
					out.WriteByte(' ')
				}
			}
			continue
		}
		// Try plain string (toolResult content).
		var s string
		if err := json.Unmarshal(entry.Message.Content, &s); err == nil && s != "" {
			out.WriteString(s)
			out.WriteByte(' ')
		}
	}
	return out.String(), nil
}

func extractFirstUserText(line string) string {
	var entry struct {
		Message *struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Message == nil {
		return ""
	}
	if entry.Message.Role != "user" {
		return ""
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(entry.Message.Content, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
		return ""
	}

	var s string
	if err := json.Unmarshal(entry.Message.Content, &s); err == nil {
		return s
	}
	return ""
}

func truncateTitle(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxLen {
		return s
	}
	cut := strings.LastIndex(s[:maxLen], " ")
	if cut < maxLen/2 {
		cut = maxLen
	}
	return s[:cut] + "…"
}


var (
	errEmpty      = &parseError{"empty file"}
	errNotSession = &parseError{"not a session header"}
)

type parseError struct{ msg string }

func (e *parseError) Error() string { return e.msg }
