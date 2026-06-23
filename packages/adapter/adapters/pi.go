package adapters

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/filewatch"
	"github.com/gmuxapp/gmux/packages/paths"
)

// Compile-time interface checks.
var (
	_ adapter.ConversationSource  = (*Pi)(nil)
	_ adapter.Launchable          = (*Pi)(nil)
	_ adapter.SessionFiler        = (*Pi)(nil)
	_ adapter.Resumer             = (*Pi)(nil)
	_ adapter.SessionExtender     = (*Pi)(nil)
	_ adapter.PassthroughDetector = (*Pi)(nil)
)

// piSubcommands are pi's one-shot CLI verbs (`pi <verb> ...`). pi recognizes
// these only at argv[1]; anywhere else they're a chat message. This list is
// CORRECTNESS-critical: gmux injects `-e` right after the binary for the
// session extension, which shoves the verb off argv[1] and demotes it to a
// prompt — so a verb missing here means `gmux -- pi <verb>` silently starts a
// chat instead of running the command. Keep synced with `pi --help`.
var piSubcommands = map[string]bool{
	"install":   true,
	"remove":    true,
	"uninstall": true,
	"update":    true,
	"list":      true,
	"config":    true,
}

// piInfoFlags short-circuit pi to print-and-exit. Passing these through is
// POLISH, not correctness: `-e` injection doesn't break them (pi still prints
// help/version), we just skip spawning a throwaway session for them.
var piInfoFlags = map[string]bool{
	"--help":    true,
	"-h":        true,
	"--version": true,
}

func init() {
	All = append(All, NewPi())
}

// Pi is the adapter for the pi coding agent. Session identity, title, and
// status are reported authoritatively by the gmux pi extension (SessionExtender;
// see pi-ext.mjs), not inferred from PTY output. See the var block above for
// the full set of implemented capabilities.
type Pi struct{}

func NewPi() *Pi { return &Pi{} }

func (p *Pi) Name() string { return "pi" }

func (p *Pi) Discover() bool {
	// Fast path: check if 'pi' binary exists on PATH without executing it.
	// Running `pi --version` is too slow (~3s for Node.js startup).
	_, err := exec.LookPath("pi")
	return err == nil
}

// piBinaryIndex returns the index of the pi binary token in args, or -1 if
// none appears before a `--` separator.
func piBinaryIndex(args []string) int {
	for i, arg := range args {
		if arg == "--" {
			return -1
		}
		if base := filepath.Base(arg); base == "pi" || base == "pi-coding-agent" {
			return i
		}
	}
	return -1
}

// Match returns true if the command invokes the `pi` or `pi-coding-agent`
// binary (before any `--` separator).
func (p *Pi) Match(cmd []string) bool {
	return piBinaryIndex(cmd) >= 0
}

// Env returns no extra environment variables.
func (p *Pi) Env(_ adapter.EnvContext) []string { return nil }

// IsPassthrough reports whether the invocation is a one-shot, non-session pi
// command rather than an interactive agent session: a subcommand (`pi update`,
// `pi list`, ...) or an info flag (`pi --help`, `pi --version`). pi recognizes
// a subcommand only as the token immediately after the binary; info flags
// short-circuit from anywhere in the top-level args.
func (p *Pi) IsPassthrough(args []string) bool {
	i := piBinaryIndex(args)
	if i < 0 {
		return false
	}
	if i+1 < len(args) && piSubcommands[args[i+1]] {
		return true
	}
	for _, rest := range args[i+1:] {
		if rest == "--" {
			break
		}
		if piInfoFlags[rest] {
			return true
		}
	}
	return false
}

// ExtendCommand splices `-e <extPath>` in right after the pi binary so pi loads
// the gmux extension. The binary may not be args[0] (e.g. `npx pi`, `env pi`),
// so we insert after the binary token, not the front — inserting at the front
// would hand -e to the wrapper. Extensions accumulate, so this coexists with
// the user's own -e flags. pi's session_start (which the extension hooks) fires
// on every bind, including the warm /resume-select that reads no file.
func (p *Pi) ExtendCommand(args []string, extPath string) []string {
	i := piBinaryIndex(args)
	if i < 0 {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, args[:i+1]...)
	out = append(out, "-e", extPath)
	return append(out, args[i+1:]...)
}

func (p *Pi) Launchers() []adapter.Launcher {
	return []adapter.Launcher{{
		ID:          "pi",
		Label:       "pi",
		Command:     []string{"pi"},
		Description: "Coding agent",
	}}
}

// Monitor is a no-op for the pi adapter — status is reported by the gmux pi
// extension (turn events over the runner socket), not inferred from PTY
// output. This avoids flicker from spinner redraws.
func (p *Pi) Monitor(_ []byte) *adapter.Event {
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
// Title priority: session_info.name > first user message > "" (no
// conversation-derived title yet; callers fall back to cwd/kind).
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
		info.Title = "" // no name and no message yet
	}

	info.Slug = adapter.Slugify(info.Title)

	return info, nil
}

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

// --- ConversationSource ---

func (p *Pi) SnapshotConversations(sink adapter.ConversationSink) {
	filewatch.Snapshot(p.SessionRootDir(), ".jsonl", sink.Upsert)
}

func (p *Pi) WatchConversations(ctx context.Context, sink adapter.ConversationSink) error {
	return filewatch.Watch(ctx, p.SessionRootDir(), ".jsonl", func(e filewatch.Event) {
		if e.Removed {
			sink.Remove(e.Path)
		} else {
			sink.Upsert(e.Path)
		}
	})
}
