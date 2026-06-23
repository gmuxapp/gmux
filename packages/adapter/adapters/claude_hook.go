package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// Claude reports session state authoritatively via a gmux command hook
// (SessionHookCommand, ADR 0011/0013). Unlike codex's `-c` config override,
// Claude Code takes hooks through settings, so we splice `--settings
// <inline-json>` after the claude binary. Per Claude's docs that is a
// highest-precedence-but-merged layer whose hook arrays concatenate across
// sources, so ours add to — never clobber — the user's hooks. The injected
// hooks run `gmux __claude-hook`, which relays an authoritative event to the
// runner socket. Launches the hook can't cover (e.g. a shell-wrapped argv where
// injection is lost) run without daemon-reported live state (no fallback).
var _ adapter.SessionHookCommand = (*Claude)(nil)

// HookCommand splices `--settings <inline-json>` in right after the claude
// binary. Any user `--settings` are folded into ours (deep-merged: arrays
// concatenate, user scalars win, but a user value can never wipe our hook
// arrays). Returns ok=false (args unchanged) if the binary isn't found or
// anything fails, so the runner launches unmodified and the daemon's fallback
// attribution applies.
func (c *Claude) HookCommand(args []string, selfBin string) ([]string, bool) {
	i := claudeBinaryIndex(args)
	if i < 0 || selfBin == "" {
		return args, false
	}
	tail, userSettings := extractClaudeSettings(args[i+1:])
	settingsJSON, err := buildClaudeSettings(selfBin, userSettings)
	if err != nil {
		return args, false
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, args[:i+1]...)
	out = append(out, "--settings", settingsJSON)
	return append(out, tail...), true
}

// claudeBinaryIndex returns the index of the `claude` binary token in args, or
// -1 if none appears before a `--` separator.
func claudeBinaryIndex(args []string) int {
	for i, arg := range args {
		if arg == "--" {
			return -1
		}
		if filepath.Base(arg) == "claude" {
			return i
		}
	}
	return -1
}

// extractClaudeSettings removes `--settings <value>` / `--settings=<value>`
// pairs from tokens, returning the filtered tokens and the collected values.
// Stops at a bare `--` so a positional prompt is never misread as a flag value.
func extractClaudeSettings(tokens []string) (filtered, values []string) {
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == "--" {
			filtered = append(filtered, tokens[i:]...)
			break
		}
		switch {
		case tok == "--settings":
			if i+1 < len(tokens) {
				values = append(values, tokens[i+1])
				i++
			}
		case strings.HasPrefix(tok, "--settings="):
			values = append(values, strings.TrimPrefix(tok, "--settings="))
		default:
			filtered = append(filtered, tok)
		}
	}
	return filtered, values
}

// buildClaudeSettings returns the inline JSON for `--settings`: gmux's session
// hooks, with any user-provided settings deep-merged on top.
func buildClaudeSettings(selfBin string, userSettings []string) (string, error) {
	var base any = claudeGmuxHooks(selfBin)
	for _, us := range userSettings {
		if obj, ok := parseClaudeSettings(us); ok {
			base = mergeClaudeSettings(base, obj)
		}
	}
	out, err := json.Marshal(base)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// claudeGmuxHooks builds the gmux hook config. Each hook runs `gmux
// __claude-hook`, which reads Claude's payload on stdin and forwards an
// authoritative event to the runner (see cmd/gmux/claudehook.go). The command
// is shell-interpreted by Claude, so the binary path is single-quoted.
//
//   - SessionStart    → bind held transcript + id + title/slug; fires on
//     startup AND resume/clear/compact, so it rebinds on /resume.
//   - UserPromptSubmit → turn start (working).
//   - Stop             → rebind + turn end completed.
//   - SessionEnd       → turn end aborted; covers Ctrl+C / exit where Stop
//     does not fire.
func claudeGmuxHooks(selfBin string) map[string]any {
	cmd := shellQuote(selfBin) + " __claude-hook"
	entry := func(timeout int) []any {
		return []any{map[string]any{
			"hooks": []any{map[string]any{
				"type":    "command",
				"command": cmd,
				"timeout": timeout,
			}},
		}}
	}
	return map[string]any{
		"hooks": map[string]any{
			"SessionStart":     entry(10),
			"UserPromptSubmit": entry(10),
			"Stop":             entry(10),
			"SessionEnd":       entry(5),
		},
	}
}

// parseClaudeSettings interprets a user `--settings` value as inline JSON
// (starts with `{`) or a path to a JSON file.
func parseClaudeSettings(value string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(value)
	var data []byte
	if strings.HasPrefix(trimmed, "{") {
		data = []byte(trimmed)
	} else {
		b, err := os.ReadFile(trimmed)
		if err != nil {
			return nil, false
		}
		data = b
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, false
	}
	return obj, true
}

// mergeClaudeSettings deep-merges over onto base. Objects merge recursively;
// arrays concatenate (user hooks run alongside gmux's); scalars from over win.
// A container in base is never replaced by a scalar/null in over — so a user
// `hooks: null` or `hooks: []` cannot wipe gmux's hook arrays.
func mergeClaudeSettings(base, over any) any {
	bm, bMap := base.(map[string]any)
	om, oMap := over.(map[string]any)
	if bMap && oMap {
		for k, ov := range om {
			if bv, ok := bm[k]; ok {
				bm[k] = mergeClaudeSettings(bv, ov)
			} else {
				bm[k] = ov
			}
		}
		return bm
	}
	ba, bArr := base.([]any)
	oa, oArr := over.([]any)
	if bArr && oArr {
		return append(ba, oa...)
	}
	if (bMap || bArr) && !(oMap || oArr) {
		return base
	}
	return over
}

// ClaudeHookBodies translates a Claude Code hook payload (JSON on the hook's
// stdin, carrying hook_event_name) into the runner's tool-neutral /hook/event
// bodies (docs/runner-hook-protocol.md). Pure apart from reading the transcript
// for the title, so it is unit-testable. Returns nil for events we don't map.
func ClaudeHookBodies(input []byte) [][]byte {
	var in struct {
		HookEventName  string `json:"hook_event_name"`
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		Source         string `json:"source"`
		SessionTitle   string `json:"session_title"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil
	}

	turn := func(phase, outcome string) []byte {
		m := map[string]string{"op": "turn", "phase": phase}
		if outcome != "" {
			m["outcome"] = outcome
		}
		b, _ := json.Marshal(m)
		return b
	}

	switch in.HookEventName {
	case "SessionStart":
		if b, ok := claudeSessionBody(in.TranscriptPath, in.SessionID, in.Source, in.SessionTitle); ok {
			return [][]byte{b}
		}
	case "UserPromptSubmit":
		return [][]byte{turn("start", "")}
	case "Stop":
		// Stop fires only on a clean finish — Claude routes interrupts/API errors
		// elsewhere (StopFailure, which we don't wire) — so "completed" is
		// accurate here (unlike codex, whose Stop can't tell completion from
		// abort). An Esc-interrupted turn stays working until the next prompt.
		if b, ok := claudeSessionBody(in.TranscriptPath, in.SessionID, "", in.SessionTitle); ok {
			return [][]byte{b, turn("end", "completed")} // refresh title/slug, then end the turn
		}
		return [][]byte{turn("end", "completed")}
	case "SessionEnd":
		return [][]byte{turn("end", "aborted")}
	}
	return nil
}

// claudeSessionBody builds the authoritative "session" bind body: the held
// transcript, id, bind reason, and a human title + explicit slug (Claude's
// session_id is a UUID that slugifies into an unreadable URL).
func claudeSessionBody(transcriptPath, sessionID, source, sessionTitle string) ([]byte, bool) {
	if transcriptPath == "" {
		return nil, false
	}
	body := map[string]any{"op": "session", "path": transcriptPath}
	if sessionID != "" {
		body["id"] = sessionID
	}
	if source != "" {
		body["reason"] = source
	}
	if title, slug := claudeTitleSlug(transcriptPath, sessionTitle); title != "" {
		body["name"] = title
		body["slug"] = slug
	}
	b, _ := json.Marshal(body)
	return b, true
}

// claudeTitleSlug resolves the title (Claude's own session_title, else the
// transcript-derived title) and a matching slug. Returns "","" when only a
// placeholder is available, so the runner keeps any better title it has.
func claudeTitleSlug(transcriptPath, sessionTitle string) (title, slug string) {
	if t := strings.TrimSpace(sessionTitle); t != "" {
		return t, adapter.Slugify(t)
	}
	info, err := NewClaude().ParseSessionFile(transcriptPath)
	if err != nil || info == nil || info.Title == "" {
		return "", ""
	}
	return info.Title, info.Slug
}
