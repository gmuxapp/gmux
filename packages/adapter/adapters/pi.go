package adapters

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// Compile-time interface checks.
var (
	_ adapter.SessionFiler = (*Pi)(nil)
	_ adapter.FileMonitor  = (*Pi)(nil)
	_ adapter.Resumer      = (*Pi)(nil)
)

func init() {
	Launchers = append(Launchers, adapter.Launcher{
		ID:          "pi",
		Label:       "pi",
		Command:     []string{"pi"},
		Description: "Coding agent",
	})
	All = append(All, NewPi())
}

// Pi is the adapter for the pi coding agent.
// Recognizes pi/pi-coding-agent commands and monitors PTY output for
// spinner patterns to report active/idle status. Implements SessionFiler,
// FileMonitor, and Resumer for session file discovery and resume.
type Pi struct{}

func NewPi() *Pi { return &Pi{} }

func (p *Pi) Name() string { return "pi" }

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

// Spinner characters used by pi's TUI (braille pattern dots).
var piSpinnerChars = [][]byte{
	[]byte("⠋"), []byte("⠙"), []byte("⠹"), []byte("⠸"),
	[]byte("⠼"), []byte("⠴"), []byte("⠦"), []byte("⠧"),
	[]byte("⠇"), []byte("⠏"),
}

// Monitor detects pi's spinner pattern in PTY output.
func (p *Pi) Monitor(output []byte) *adapter.Status {
	for _, sc := range piSpinnerChars {
		if idx := bytes.Index(output, sc); idx >= 0 {
			rest := output[idx+len(sc):]
			if bytes.Contains(rest, []byte("Working")) {
				return &adapter.Status{
					Label: "working",
					State: "active",
				}
			}
		}
	}
	return nil
}

// --- SessionFiler ---

// SessionDir returns pi's session directory for a given cwd.
// Pi encodes: strip leading /, replace remaining / with -, wrap in --.
// /home/mg/dev/gmux → --home-mg-dev-gmux--
func (p *Pi) SessionDir(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := strings.TrimPrefix(cwd, "/")
	encoded := "--" + strings.ReplaceAll(path, "/", "-") + "--"
	return filepath.Join(home, ".pi", "agent", "sessions", encoded)
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

	return info, nil
}

// --- FileMonitor ---

// ParseNewLines receives lines appended to an attributed session file
// and returns events for meaningful changes.
func (p *Pi) ParseNewLines(lines []string) []adapter.FileEvent {
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
		}
	}
	return events
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

// ExtractPiText reads a pi JSONL session file and extracts conversation
// text suitable for similarity matching (ADR-0009).
func ExtractPiText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" || !strings.Contains(line, `"text"`) {
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
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(entry.Message.Content, &blocks); err != nil {
			var s string
			if err := json.Unmarshal(entry.Message.Content, &s); err == nil {
				out.WriteString(s)
				out.WriteByte(' ')
			}
			continue
		}
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				out.WriteString(b.Text)
				out.WriteByte(' ')
			}
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
