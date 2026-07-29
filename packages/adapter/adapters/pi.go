package adapters

import (
	"context"
	"encoding/json"
	"io"
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
	_ adapter.ConversationSource    = (*Pi)(nil)
	_ adapter.ConversationProber    = (*Pi)(nil)
	_ adapter.Launchable            = (*Pi)(nil)
	_ adapter.ConversationDescriber = (*Pi)(nil)
	_ adapter.ConversationOpener    = (*Pi)(nil)
	_ adapter.Resumer               = (*Pi)(nil)
	_ adapter.SessionExtender       = (*Pi)(nil)
	_ adapter.PassthroughDetector   = (*Pi)(nil)
	_ adapter.AgentActionEncoder    = (*Pi)(nil)
	_ adapter.AgentLauncher         = (*Pi)(nil)
)

// piSubcommands are pi's one-shot CLI verbs (`pi <verb> ...`). pi recognizes
// these only at argv[1]; anywhere else they're a chat message. This list is
// CORRECTNESS-critical: gmux injects `-e` right after the binary for the
// session extension, which shoves the verb off argv[1] and demotes it to a
// prompt — so a verb missing here means `gmux -- pi <verb>` silently starts a
// chat instead of running the command. Keep synced with `pi --help`.
var piSubcommands = map[string]bool{
	// auth was added after pi 0.82.1. Keeping it in the passthrough superset is
	// harmless with older pi (where it is not a valid command) and prevents a
	// newer pi's one-shot auth flow from being silently demoted to a chat prompt.
	"auth":      true,
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

// piActionReadyTimeout bounds how long a caller waits for pi to report
// itself ready before abandoning a semantic action. pi is a Node.js
// program: cold start (interpreter boot, module load, TUI setup) is
// seconds, not milliseconds, so a short timeout would spuriously fail
// freshly launched or resumed sessions.
const piActionReadyTimeout = 10 * time.Second

// EncodeAction maps gmux's semantic turn-control actions to pi's
// composer keybinds:
//
//   - ActionSend is Enter — submits, delivering into the current turn
//     immediately when pi is streaming;
//   - ActionSendAfterTurn is Alt+Enter — queues until the current turn
//     ends, and acts as a plain submit when pi is idle;
//   - ActionInterrupt is Escape — pi's interactive-mode onEscape aborts
//     the streaming turn (dist/modes/interactive/interactive-mode.js
//     restoreQueuedMessagesToEditor({abort:true})).
//
// Two pi-specific limitations callers should know about:
//
//   - Interrupt is not a clean stop: the same pi handler *restores any
//     queued follow-ups into the composer*, so after a cancel the
//     composer may hold text the user never retyped, which a
//     subsequent submit would send along with the new prompt.
//   - Both non-Enter encodings target pi's *default* keybindings.
//     Composer keybinds are user-configurable, so a session whose user
//     remapped alt+enter or escape silently loses semantic follow-up /
//     interrupt support: the bytes still arrive, they just no longer
//     mean that action. Enter is not realistically remappable, so
//     ActionSend is safe.
//
// Encodings are chosen to parse correctly regardless of which keyboard
// protocol pi negotiated with its startup-attached terminal:
//
//   - Enter is "\r": pi-tui parses a bare CR as "enter" in both its
//     legacy and Kitty-protocol modes.
//   - Alt+Enter is the Kitty CSI-u encoding "\x1b[13;3u" (codepoint 13,
//     modifier 3 = 1+alt), NOT the legacy ESC CR ("\x1b\r"): pi-tui
//     tries CSI-u parsing unconditionally, so this reads as alt+enter
//     in both modes — whereas ESC CR is misparsed as shift+enter
//     (newline, no submit) whenever pi negotiated the Kitty protocol,
//     which happens for any `gmux -- pi` started in the foreground of a
//     Kitty-protocol terminal (kitty, ghostty, wezterm, foot). Both
//     behaviors verified against pi-tui's parseKey and live sessions.
//   - Escape is the Kitty CSI-u encoding "\x1b[27u" (codepoint 27, no
//     modifier), not a bare "\x1b". pi-tui's escape matcher accepts
//     both in both modes (`data === "\x1b" || matchesKittySequence(data,
//     27, 0)` in keys.js), but a lone ESC byte is the prefix of every
//     escape sequence: if any bytes follow in the same read, pi parses
//     the combination as some other key. CSI-u is self-delimiting and
//     therefore unambiguous.
func (p *Pi) EncodeAction(action adapter.AgentAction) (string, bool) {
	switch action {
	case adapter.ActionSend:
		return "\r", true
	case adapter.ActionSendAfterTurn:
		return "\x1b[13;3u", true
	case adapter.ActionInterrupt:
		return "\x1b[27u", true
	}
	return "", false
}

// ActionReadyTimeout implements adapter.AgentActionEncoder.
func (p *Pi) ActionReadyTimeout() time.Duration { return piActionReadyTimeout }

// LaunchCommand implements adapter.AgentLauncher: a bare interactive pi,
// plus the two knobs `gmux agent prompt --new` exposes.
//
// The flags are pi's own long spellings, verified against `pi --help`:
// `--model <pattern>` takes a model pattern or id, `--name <name>` sets the
// session display name. Long forms deliberately — pi's `-n` is --name but a
// single-letter flag is the kind of thing an agent reshuffles between
// releases, and this argv is stored on the session forever.
//
// No prompt is ever encoded here: the first prompt travels the same
// readiness-gated semantic path as every later one, so a freshly launched
// session has one health event (admission) rather than a launch-shaped
// special case. An empty option is omitted entirely rather than passed empty:
// `--model ""` asks pi for a model named "", which is not the same request as
// asking for pi's default.
func (p *Pi) LaunchCommand(opts adapter.LaunchOptions) ([]string, bool) {
	argv := []string{"pi"}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	if opts.Name != "" {
		argv = append(argv, "--name", opts.Name)
	}
	return argv, true
}

// --- Conversation storage (file-backed: refs are absolute JSONL paths) ---

// ConversationRootDir returns pi's top-level sessions directory.
// Respects PI_CODING_AGENT_DIR (pi's own env var for overriding the
// agent data directory, default ~/.pi/agent). This lets dev instances
// use an isolated session store.
func (p *Pi) ConversationRootDir() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}

// ConversationGone anchors deletion detection on ConversationRootDir
// (PI_CODING_AGENT_DIR/sessions or ~/.pi/agent/sessions): present root
// means a missing conversation file was deleted; absent root means the
// storage is unavailable. Refs are conversation-file paths for pi.
func (p *Pi) ConversationGone(ref string) (gone bool, ok bool) {
	return adapter.ConversationGoneAtRoot(ref, p.ConversationRootDir())
}

// ConversationDir returns pi's session directory for a given cwd.
// Pi encodes: strip leading /, replace remaining / with -, wrap in --.
// /home/mg/dev/gmux → --home-mg-dev-gmux--
func (p *Pi) ConversationDir(cwd string) string {
	root := p.ConversationRootDir()
	if root == "" {
		return ""
	}
	abs := paths.NormalizePath(cwd)
	path := strings.TrimPrefix(abs, "/")
	encoded := "--" + strings.ReplaceAll(path, "/", "-") + "--"
	return filepath.Join(root, encoded)
}

// DescribeConversation reads a pi JSONL conversation file (the ref is the
// absolute file path) and returns display metadata.
// Title priority: session_info.name > first user message > "" (no
// conversation-derived title yet; callers fall back to cwd/adapter).
func (p *Pi) DescribeConversation(ref string) (*adapter.ConversationInfo, error) {
	path := ref
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

	info := &adapter.ConversationInfo{
		ID:           header.ID,
		Cwd:          header.Cwd,
		Created:      created,
		LastActivity: fileLastActivity(path),
		Ref:          path,
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

// ResumeCommand returns the command to resume a pi session, or nil when the
// conversation has no messages worth resuming.
func (p *Pi) ResumeCommand(info *adapter.ConversationInfo) []string {
	if info == nil || info.MessageCount == 0 {
		return nil
	}
	return []string{"pi", "--session", info.Ref, "-c"}
}

// OpenConversation streams the raw JSONL transcript at ref.
func (p *Pi) OpenConversation(ref string) (io.ReadCloser, error) {
	return os.Open(ref)
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
	filewatch.Snapshot(p.ConversationRootDir(), ".jsonl", sink.Upsert)
}

func (p *Pi) WatchConversations(ctx context.Context, sink adapter.ConversationSink) error {
	return filewatch.Watch(ctx, p.ConversationRootDir(), ".jsonl", func(e filewatch.Event) {
		if e.Removed {
			sink.Remove(e.Path)
		} else {
			sink.Upsert(e.Path)
		}
	})
}
