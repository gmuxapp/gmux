package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeClaudeTranscript writes a minimal Claude JSONL whose first user message
// yields a known title/slug ("Hello world" → "hello-world").
func writeClaudeTranscript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conv.jsonl")
	line := `{"type":"user","sessionId":"abc","cwd":"/tmp","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"Hello world"}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeBodies(t *testing.T, bodies [][]byte) []map[string]any {
	t.Helper()
	out := make([]map[string]any, len(bodies))
	for i, b := range bodies {
		if err := json.Unmarshal(b, &out[i]); err != nil {
			t.Fatalf("body %d not JSON: %v", i, err)
		}
	}
	return out
}

func TestClaudeHookBodies_SessionStartBindsWithTitleSlug(t *testing.T) {
	tr := writeClaudeTranscript(t)
	in, _ := json.Marshal(map[string]string{
		"hook_event_name": "SessionStart",
		"session_id":      "uuid-1",
		"transcript_path": tr,
		"source":          "resume",
	})
	got := decodeBodies(t, ClaudeHookBodies(in))
	if len(got) != 1 {
		t.Fatalf("want 1 body, got %d", len(got))
	}
	b := got[0]
	if b["op"] != "session" || b["path"] != tr || b["id"] != "uuid-1" || b["reason"] != "resume" {
		t.Fatalf("bind body wrong: %v", b)
	}
	if b["name"] != "Hello world" || b["slug"] != "hello-world" {
		t.Fatalf("want derived title/slug, got name=%v slug=%v", b["name"], b["slug"])
	}
}

func TestClaudeHookBodies_SessionTitleWins(t *testing.T) {
	tr := writeClaudeTranscript(t)
	in, _ := json.Marshal(map[string]string{
		"hook_event_name": "SessionStart",
		"transcript_path": tr,
		"session_title":   "Custom Title",
	})
	b := decodeBodies(t, ClaudeHookBodies(in))[0]
	if b["name"] != "Custom Title" || b["slug"] != "custom-title" {
		t.Fatalf("session_title should win: name=%v slug=%v", b["name"], b["slug"])
	}
}

func TestClaudeHookBodies_TurnLifecycle(t *testing.T) {
	start := decodeBodies(t, ClaudeHookBodies([]byte(`{"hook_event_name":"UserPromptSubmit"}`)))
	if len(start) != 1 || start[0]["op"] != "turn" || start[0]["phase"] != "start" {
		t.Fatalf("UserPromptSubmit → turn start, got %v", start)
	}
	end := decodeBodies(t, ClaudeHookBodies([]byte(`{"hook_event_name":"SessionEnd"}`)))
	if len(end) != 1 || end[0]["phase"] != "end" || end[0]["outcome"] != "aborted" {
		t.Fatalf("SessionEnd → turn end aborted, got %v", end)
	}
}

func TestClaudeHookBodies_StopRefreshesThenEnds(t *testing.T) {
	tr := writeClaudeTranscript(t)
	in, _ := json.Marshal(map[string]string{"hook_event_name": "Stop", "transcript_path": tr})
	got := decodeBodies(t, ClaudeHookBodies(in))
	if len(got) != 2 || got[0]["op"] != "session" || got[1]["op"] != "turn" || got[1]["outcome"] != "completed" {
		t.Fatalf("Stop → [session, turn end completed], got %v", got)
	}
}

func TestClaudeHookBodies_UnknownEventNil(t *testing.T) {
	if got := ClaudeHookBodies([]byte(`{"hook_event_name":"PreToolUse"}`)); got != nil {
		t.Fatalf("unknown event should map to nil, got %v", got)
	}
}

func TestClaudeHookBodies_NoTranscriptNoBind(t *testing.T) {
	// SessionStart without a transcript path has nothing authoritative to bind.
	if got := ClaudeHookBodies([]byte(`{"hook_event_name":"SessionStart"}`)); got != nil {
		t.Fatalf("no transcript → no bind, got %v", got)
	}
}

func TestClaudeHookBodies_MalformedNil(t *testing.T) {
	if got := ClaudeHookBodies([]byte("not json")); got != nil {
		t.Fatalf("malformed payload → nil, got %v", got)
	}
}

func TestClaudeHookBodies_BindsBeforeTranscriptHasContent(t *testing.T) {
	// SessionStart can fire before the transcript is written. We still bind the
	// held path + id authoritatively; title/slug are omitted (runner falls back
	// to the id) and get filled in when Stop re-binds after the first turn.
	in, _ := json.Marshal(map[string]string{
		"hook_event_name": "SessionStart",
		"session_id":      "uuid-2",
		"transcript_path": filepath.Join(t.TempDir(), "missing.jsonl"),
	})
	b := decodeBodies(t, ClaudeHookBodies(in))[0]
	if b["op"] != "session" || b["id"] != "uuid-2" {
		t.Fatalf("want authoritative bind, got %v", b)
	}
	if _, hasName := b["name"]; hasName {
		t.Fatalf("no title until transcript has content, got %v", b)
	}
}

func TestClaudeHookCommand_Splices(t *testing.T) {
	out, ok := (&Claude{}).HookCommand([]string{"claude", "--model", "opus"}, "/usr/bin/gmux")
	if !ok {
		t.Fatal("expected ok")
	}
	// --settings is spliced right after the binary, carrying our hooks.
	if out[0] != "claude" || out[1] != "--settings" {
		t.Fatalf("want claude --settings ..., got %v", out[:2])
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(out[2]), &settings); err != nil {
		t.Fatalf("settings not JSON: %v", err)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if _, has := hooks["SessionStart"]; !has {
		t.Fatalf("missing SessionStart hook: %v", settings)
	}
	if out[len(out)-2] != "--model" || out[len(out)-1] != "opus" {
		t.Fatalf("user args dropped: %v", out)
	}
}

func TestClaudeHookCommand_NoBinaryNoInject(t *testing.T) {
	args := []string{"bash", "-c", "echo hi"}
	out, ok := (&Claude{}).HookCommand(args, "/usr/bin/gmux")
	if ok {
		t.Fatalf("no claude binary → ok=false, got %v", out)
	}
}

func TestClaudeHookCommand_MergesUserSettings(t *testing.T) {
	out, ok := (&Claude{}).HookCommand(
		[]string{"claude", "--settings", `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"user"}]}]},"model":"opus"}`},
		"/usr/bin/gmux",
	)
	if !ok {
		t.Fatal("expected ok")
	}
	s := mergedSettings(t, out)
	if s["model"] != "opus" {
		t.Fatalf("user scalar lost: %v", s)
	}
	if n := sessionStartHookCount(s); n != 2 {
		t.Fatalf("hook arrays should concatenate (gmux + user), got %d: %v", n, s)
	}
}

// mergedSettings asserts exactly one --settings flag survives in out and
// returns its parsed JSON (the single combined layer Claude honors).
func mergedSettings(t *testing.T, out []string) map[string]any {
	t.Helper()
	n := 0
	var merged string
	for i, a := range out {
		if a == "--settings" {
			n++
			merged = out[i+1]
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one --settings, got %d: %v", n, out)
	}
	var s map[string]any
	if err := json.Unmarshal([]byte(merged), &s); err != nil {
		t.Fatalf("merged --settings not JSON: %v", err)
	}
	return s
}

func parseJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	return m
}

func sessionStartHookCount(s map[string]any) int {
	hooks, _ := s["hooks"].(map[string]any)
	starts, _ := hooks["SessionStart"].([]any)
	return len(starts)
}

func TestClaudeHookCommand_MergesUserSettingsFile(t *testing.T) {
	// A user --settings given as a *file path* (not inline JSON) must be read
	// and merged — the branch the greptile "trimmed path" note lived in.
	userFile := filepath.Join(t.TempDir(), "user.json")
	if err := os.WriteFile(userFile, []byte(`{"model":"haiku","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"user"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, ok := (&Claude{}).HookCommand([]string{"claude", "--settings", userFile}, "/usr/bin/gmux")
	if !ok {
		t.Fatal("expected ok")
	}
	s := mergedSettings(t, out)
	if s["model"] != "haiku" {
		t.Fatalf("settings file not read/merged: %v", s)
	}
	if n := sessionStartHookCount(s); n != 2 {
		t.Fatalf("want gmux + user SessionStart hooks, got %d", n)
	}
}

func TestClaudeHookCommand_MergesEqualsForm(t *testing.T) {
	out, ok := (&Claude{}).HookCommand([]string{"claude", `--settings={"model":"opus"}`}, "/usr/bin/gmux")
	if !ok {
		t.Fatal("expected ok")
	}
	if s := mergedSettings(t, out); s["model"] != "opus" {
		t.Fatalf("--settings=<inline> not merged: %v", s)
	}
}

func TestClaudeHookCommand_StopsAtDoubleDash(t *testing.T) {
	// Tokens after `--` are positional and must be passed through verbatim — a
	// `--settings` there is a prompt, not a flag to extract.
	out, ok := (&Claude{}).HookCommand([]string{"claude", "--", "--settings", "hi"}, "/usr/bin/gmux")
	if !ok {
		t.Fatal("expected ok")
	}
	// Our --settings is injected right after the binary; the post-`--` tokens
	// (which include a literal "--settings hi") survive verbatim as positionals.
	if out[1] != "--settings" || sessionStartHookCount(parseJSON(t, out[2])) != 1 {
		t.Fatalf("gmux --settings not injected after binary: %v", out)
	}
	if joined := strings.Join(out, " "); !strings.HasSuffix(joined, "-- --settings hi") {
		t.Fatalf("positional tokens after -- altered: %v", out)
	}
}

func TestClaudeHookCommand_UserCannotWipeHooks(t *testing.T) {
	// A user `hooks: null` must not erase gmux's hooks (our attribution would
	// silently break). The deep-merge keeps gmux's container over a null.
	out, ok := (&Claude{}).HookCommand([]string{"claude", "--settings", `{"hooks":null}`}, "/usr/bin/gmux")
	if !ok {
		t.Fatal("expected ok")
	}
	if n := sessionStartHookCount(mergedSettings(t, out)); n != 1 {
		t.Fatalf("gmux SessionStart hook must survive hooks:null, got %d", n)
	}
}
