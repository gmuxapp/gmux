package adapters

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/filewatch"
)

// Compile-time interface checks.
var (
	_ adapter.ConversationSource = (*Codex)(nil)
	_ adapter.Launchable         = (*Codex)(nil)
	_ adapter.SessionFiler       = (*Codex)(nil)
	_ adapter.SessionHookCommand = (*Codex)(nil)
	_ adapter.Resumer            = (*Codex)(nil)
)

func init() {
	All = append(All, NewCodex())
}

// Codex is the adapter for OpenAI Codex CLI.
// Sessions are JSONL files in ~/.codex/sessions/YYYY/MM/DD/.
//
// Live session state is reported authoritatively by the gmux codex hook
// (SessionHookCommand; see HookCommand + CodexHookBodies): gmux injects the
// hook per-launch via codex's `-c hooks.<Event>=...` config overrides, so the
// hook runs `gmux __codex-hook <Event>` and POSTs to the runner socket on every
// SessionStart/UserPromptSubmit/Stop — ephemeral, with no mutation of the
// user's ~/.codex. When codex is too old to support hooks, nothing is injected
// and the session runs without daemon-reported live state (there is no
// metadata-attribution fallback).
type Codex struct {
	hooksOnce sync.Once
	hooksOK   bool
}

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

// Monitor is a no-op — status is driven by the agent hook.
func (c *Codex) Monitor(_ []byte) *adapter.Event {
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
// session files. Codex organizes by date (YYYY/MM/DD), not by cwd; the
// ConversationSource walks the whole tree for historical sessions.
func (c *Codex) SessionDir(_ string) string {
	root := c.SessionRootDir()
	if root == "" {
		return ""
	}
	now := time.Now()
	return filepath.Join(root, now.Format("2006"), now.Format("01"), now.Format("02"))
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
				Type    string          `json:"type"`
				Role    string          `json:"role"`
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

	info.Slug = adapter.Slugify(info.Title)

	return info, nil
}

// --- SessionHookCommand ---

// codexMinHookVersion is the floor codex version for the gmux hook integration.
// By v0.135 codex's command-hook system is documented and stable, and the hook
// config + per-hook trust-state shapes we depend on (`-c hooks.<Event>=...` and
// `hooks.state.<key>.trusted_hash`) are present. Below this we inject nothing
// (an older codex would reject the -c hooks overrides, breaking launch); such
// sessions run without daemon-reported live state.
const codexMinHookVersion = "0.135.0"

// codexHookEvents are the codex hook events the gmux hook subscribes to, mapped
// onto the tool-neutral /hook/event protocol in CodexHookBodies:
//
//	SessionStart     → op "session" (bind: transcript_path + title/slug)
//	UserPromptSubmit → op "turn" phase "start"
//	Stop             → op "session" (title refresh) + op "turn" phase "end"
var codexHookEvents = []string{"SessionStart", "UserPromptSubmit", "Stop"}

// codexSessionFlagsSource is the synthetic source path codex assigns to hooks
// defined via `-c` overrides (the SessionFlags config layer): codex builds it as
// AbsolutePathBuf::resolve_path_against_base("<session-flags>/config.toml", "/"),
// which displays as this on POSIX. It is the `key_source` half of the per-hook
// trust-state key, so we must reproduce it byte-for-byte (see codexHookTrustKey).
const codexSessionFlagsSource = "/<session-flags>/config.toml"

// hooksSupported reports whether the installed codex is recent enough for the
// hook integration. Memoized: `codex --version` is cheap (Rust) but the runner
// calls HookCommand on every launch.
func (c *Codex) hooksSupported() bool {
	c.hooksOnce.Do(func() {
		out, err := exec.Command("codex", "--version").Output()
		if err != nil {
			return
		}
		if v := parseCodexVersion(string(out)); v != nil {
			c.hooksOK = !semverLess(v, mustParseSemver(codexMinHookVersion))
		}
	})
	return c.hooksOK
}

// HookCommand injects the gmux codex hook per-launch via `-c` config overrides
// (a SessionFlags config layer) — ephemeral, like pi's `-e`, with no mutation of
// the user's ~/.codex. Returns ok=false (args unchanged) when codex is too old
// to support hooks.
func (c *Codex) HookCommand(args []string, selfBin string) ([]string, bool) {
	if !c.hooksSupported() {
		return args, false
	}
	out := codexHookArgs(args, selfBin)
	return out, len(out) > len(args)
}

// codexHookArgs splices, right after the codex binary token (which may not be
// args[0], e.g. `env codex`), one `-c hooks.<Event>=...` override per subscribed
// event plus a single `-c hooks.state=...` that pre-trusts *only those* hooks.
//
// Trust is scoped deliberately: a CLI-injected hook is non-managed and therefore
// Untrusted, which codex silently skips. Rather than --dangerously-bypass-hook-
// trust (a process-global flag that would un-gate the user's, plugins', and
// already-trusted projects' hooks too), we inject the exact per-hook
// `trusted_hash` codex computes, so only gmux's own benign reporting hooks are
// trusted and codex's trust model is preserved for everything else. If our hash
// ever fails to match (e.g. a codex internal change), the hook is simply
// Untrusted → skipped → the session runs without hook-reported state. It
// degrades; it never broadens trust.
//
// Returns args unchanged if no codex binary token is found.
func codexHookArgs(args []string, selfBin string) []string {
	i := codexBinaryIndex(args)
	if i < 0 {
		return args
	}
	injected := make([]string, 0, 2*len(codexHookEvents)+2)
	stateEntries := make([]string, 0, len(codexHookEvents))
	for _, event := range codexHookEvents {
		inner := codexHookCommand(selfBin, event)
		label := codexEventLabel(event)
		injected = append(injected, "-c", fmt.Sprintf(
			`hooks.%s=[{hooks=[{type="command",command=%s,timeout=5}]}]`,
			event, tomlString(inner)))
		stateEntries = append(stateEntries, fmt.Sprintf("%s={trusted_hash=%s}",
			tomlString(codexHookTrustKey(label)), tomlString(codexHookTrustedHash(inner, label))))
	}
	injected = append(injected, "-c", "hooks.state={"+strings.Join(stateEntries, ",")+"}")
	out := make([]string, 0, len(args)+len(injected))
	out = append(out, args[:i+1]...)
	out = append(out, injected...)
	return append(out, args[i+1:]...)
}

// codexHookCommand is the shell command codex runs for an event hook (via
// `sh -lc`): the gmux binary (shell-quoted) relaying that event. This exact
// string is both the hook's `command` and the input to its trust hash.
func codexHookCommand(selfBin, event string) string {
	return shellQuote(selfBin) + " __codex-hook " + event
}

// codexEventLabel maps a codex hook event name to the snake_case label codex
// uses in hook keys (hook_event_key_label).
func codexEventLabel(event string) string {
	switch event {
	case "SessionStart":
		return "session_start"
	case "UserPromptSubmit":
		return "user_prompt_submit"
	case "Stop":
		return "stop"
	default:
		return strings.ToLower(event)
	}
}

// codexHookTrustKey reproduces codex's per-hook trust-state key for a hook the
// gmux launcher injects via `-c`: `{key_source}:{event_label}:{group}:{handler}`
// (hook_key). Our hook is the sole matcher group (0) with a single handler (0)
// in the SessionFlags layer.
func codexHookTrustKey(eventLabel string) string {
	return codexSessionFlagsSource + ":" + eventLabel + ":0:0"
}

// codexHookTrustedHash reproduces codex's `version_for_toml` over the normalized
// hook identity (discovery.command_hook_hash): sha256, hex, "sha256:"-prefixed,
// of the canonical (sorted-key, compact, no HTML escaping) JSON of
//
//	{event_name, hooks:[{type:"command", command, timeout:5, async:false}]}
//
// matcher is None for our events (matcher_pattern_for_event), and the other
// Option fields are None, so the toml→json value omits them. command is the
// pre-env-substitution command (we inject no env, so it is exactly `command`).
func codexHookTrustedHash(command, eventLabel string) string {
	identity := map[string]any{
		"event_name": eventLabel,
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": command,
			"timeout": 5,
			"async":   false,
		}},
	}
	// Match serde_json: keys sorted (Go sorts map keys), compact, and NOT
	// HTML-escaped. json.Encoder appends a newline, which serde_json::to_vec
	// does not — trim it before hashing.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(identity)
	sum := sha256.Sum256(bytes.TrimRight(buf.Bytes(), "\n"))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// codexBinaryIndex returns the index of the codex binary token in args (before
// any `--`), or -1. Mirrors piBinaryIndex: the binary may not be args[0]
// (e.g. `env codex`, `npx codex`).
func codexBinaryIndex(args []string) int {
	for i, arg := range args {
		if arg == "--" {
			return -1
		}
		if filepath.Base(arg) == "codex" {
			return i
		}
	}
	return -1
}

// tomlString renders s as a TOML basic string (double-quoted), escaping
// backslashes and double quotes.
func tomlString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// shellQuote single-quotes s for a POSIX shell (codex runs hook commands via
// `sh -lc "<command>"`). Embedded single quotes are escaped as '\”.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CodexHookBodies translates a codex hook invocation into zero or more
// tool-neutral gmux /hook/event bodies (posted in order by `gmux __codex-hook`).
// eventName is the codex hook event; input is the JSON codex wrote to the hook's
// stdin (its SessionStart/Stop input carries session_id, transcript_path, cwd).
//
// codex has no title/slug field of its own, so this derives them by parsing the
// transcript's first user prompt (reusing ParseSessionFile) and reports them as
// the session title + an explicit slug — codex's session_id is a UUID that would
// slugify into an unreadable URL.
func CodexHookBodies(eventName string, input []byte) [][]byte {
	switch eventName {
	case "SessionStart":
		if b, ok := codexSessionBody(input); ok {
			return [][]byte{b}
		}
	case "UserPromptSubmit":
		b, _ := json.Marshal(map[string]string{"op": "turn", "phase": "start"})
		return [][]byte{b}
	case "Stop":
		// outcome hardcoded "completed": codex's Stop payload has no exit-reason
		// field, so the hook can't distinguish a clean completion from an
		// interrupt/error (a user abort shows as completed+unread). Revisit if
		// codex adds an outcome-bearing Stop field.
		turn, _ := json.Marshal(map[string]string{
			"op": "turn", "phase": "end", "outcome": "completed",
		})
		if b, ok := codexSessionBody(input); ok {
			return [][]byte{b, turn} // refresh title/slug, then end the turn
		}
		return [][]byte{turn}
	}
	return nil
}

// codexSessionBody builds an op "session" body from a codex hook payload. Returns
// ok=false when there is no transcript path yet (a brand-new session before its
// file is written) — nothing to attribute.
func codexSessionBody(input []byte) ([]byte, bool) {
	var in struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		Cwd            string `json:"cwd"`
		Source         string `json:"source"`
	}
	if err := json.Unmarshal(input, &in); err != nil || in.TranscriptPath == "" {
		return nil, false
	}
	body := map[string]any{
		"op":   "session",
		"path": in.TranscriptPath,
	}
	if in.SessionID != "" {
		body["id"] = in.SessionID
	}
	if in.Cwd != "" {
		body["cwd"] = in.Cwd
	}
	if in.Source != "" {
		body["reason"] = in.Source
	}
	// Derive a human title + slug from the transcript's first user prompt.
	if title := codexHookTitle(in.TranscriptPath); title != "" {
		body["name"] = title
		body["slug"] = adapter.Slugify(title)
	}
	b, _ := json.Marshal(body)
	return b, true
}

// codexHookTitle returns a codex session's display title — the first non-system
// user prompt, truncated — or "" if the transcript has none yet.
//
// Unlike ParseSessionFile it early-exits at the first user message instead of
// reading the whole file. This matters because the hook derives the title on
// every turn-end (codex blocks on the Stop hook), the title never changes after
// the first prompt, and the transcript grows each turn: a full re-parse would be
// O(file) per turn, O(n²) over a session. The first prompt sits near the top, so
// this stays effectively O(1).
func codexHookTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// codex transcript lines (tool output, assistant messages) can be large;
	// raise the line cap well above bufio's 64KiB default.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
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
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		if entry.Type == "response_item" && entry.Payload.Role == "user" && entry.Payload.Type == "message" {
			if text := extractCodexUserText(entry.Payload.Content); text != "" {
				return truncateTitle(text, 80)
			}
		}
	}
	return ""
}

// --- semver (codex --version gating) ---

// parseCodexVersion extracts the first dotted numeric version from `codex
// --version` output (e.g. "codex-cli 0.142.0\n" → [0 142 0]). Pre-release/build
// suffixes ("-alpha.9") are ignored. Returns nil if no version is found.
func parseCodexVersion(s string) []int {
	for _, tok := range strings.Fields(s) {
		// Trim a leading 'v' and any pre-release/build suffix.
		tok = strings.TrimPrefix(tok, "v")
		if i := strings.IndexAny(tok, "-+"); i >= 0 {
			tok = tok[:i]
		}
		if v := parseSemver(tok); v != nil {
			return v
		}
	}
	return nil
}

// parseSemver parses "MAJOR.MINOR.PATCH" (missing components default to 0).
// Returns nil if the first component isn't numeric.
func parseSemver(s string) []int {
	parts := strings.Split(s, ".")
	if len(parts) == 0 {
		return nil
	}
	out := make([]int, 3)
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			if i == 0 {
				return nil
			}
			break
		}
		out[i] = n
	}
	return out
}

func mustParseSemver(s string) []int {
	v := parseSemver(s)
	if v == nil {
		panic("invalid semver constant: " + s)
	}
	return v
}

// semverLess reports whether a < b (component-wise, major.minor.patch).
func semverLess(a, b []int) bool {
	for i := 0; i < 3; i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			return av < bv
		}
	}
	return false
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

// --- ConversationSource ---

func (c *Codex) SnapshotConversations(sink adapter.ConversationSink) {
	filewatch.Snapshot(c.SessionRootDir(), ".jsonl", sink.Upsert)
}

func (c *Codex) WatchConversations(ctx context.Context, sink adapter.ConversationSink) error {
	return filewatch.Watch(ctx, c.SessionRootDir(), ".jsonl", func(e filewatch.Event) {
		if e.Removed {
			sink.Remove(e.Path)
		} else {
			sink.Upsert(e.Path)
		}
	})
}
