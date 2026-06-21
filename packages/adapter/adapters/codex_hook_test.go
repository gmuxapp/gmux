package adapters

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestParseCodexVersion(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"codex-cli 0.142.0\n", []int{0, 142, 0}},
		{"codex 0.135.0", []int{0, 135, 0}},
		{"0.142.0-alpha.9", []int{0, 142, 0}},
		{"codex v1.2.3", []int{1, 2, 3}},
		{"codex 0.13", []int{0, 13, 0}},
		{"no version here", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := parseCodexVersion(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseCodexVersion(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSemverLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.134.0", "0.135.0", true},
		{"0.135.0", "0.135.0", false},
		{"0.135.1", "0.135.0", false},
		{"0.135.0", "0.134.9", false},
		{"0.142.0", "0.135.0", false},
		{"1.0.0", "0.135.0", false},
	}
	for _, c := range cases {
		got := semverLess(mustParseSemver(c.a), mustParseSemver(c.b))
		if got != c.want {
			t.Errorf("semverLess(%s,%s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestCodexHookBodies_SessionStart(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "rollout-2026-06-20-abc.jsonl")
	writeCodexTranscript(t, transcript, "Fix the auth bug")

	input := mustJSON(t, map[string]any{
		"hook_event_name": "SessionStart",
		"session_id":      "019cfb54-dead-beef-cafe-000000000001",
		"transcript_path": transcript,
		"cwd":             "/proj",
		"source":          "startup",
	})
	bodies := CodexHookBodies("SessionStart", input)
	if len(bodies) != 1 {
		t.Fatalf("want 1 body, got %d", len(bodies))
	}
	var ev map[string]any
	mustUnmarshal(t, bodies[0], &ev)
	if ev["op"] != "session" || ev["path"] != transcript {
		t.Fatalf("bad session body: %v", ev)
	}
	// codex's UUID session_id must not become the slug; a title-derived slug is.
	if ev["slug"] == nil || strings.Contains(ev["slug"].(string), "019cfb54") {
		t.Errorf("slug should be title-derived, got %v", ev["slug"])
	}
	if ev["name"] != "Fix the auth bug" {
		t.Errorf("name = %v, want title from first user prompt", ev["name"])
	}
}

func TestCodexHookBodies_NewSessionNoTranscript(t *testing.T) {
	// A brand-new session whose transcript path is empty: nothing to attribute.
	input := mustJSON(t, map[string]any{
		"hook_event_name": "SessionStart",
		"session_id":      "abc",
		"transcript_path": "",
		"source":          "startup",
	})
	if bodies := CodexHookBodies("SessionStart", input); len(bodies) != 0 {
		t.Fatalf("want no bodies for empty transcript, got %v", bodies)
	}
}

func TestCodexHookBodies_TurnLifecycle(t *testing.T) {
	start := CodexHookBodies("UserPromptSubmit", []byte(`{}`))
	if len(start) != 1 {
		t.Fatalf("UserPromptSubmit: want 1 body, got %d", len(start))
	}
	var s map[string]string
	mustUnmarshal(t, start[0], &s)
	if s["op"] != "turn" || s["phase"] != "start" {
		t.Errorf("UserPromptSubmit body = %v", s)
	}

	// Stop with no transcript still ends the turn (completed).
	stop := CodexHookBodies("Stop", []byte(`{}`))
	if len(stop) != 1 {
		t.Fatalf("Stop(no transcript): want 1 body, got %d", len(stop))
	}
	var e map[string]string
	mustUnmarshal(t, stop[0], &e)
	if e["op"] != "turn" || e["phase"] != "end" || e["outcome"] != "completed" {
		t.Errorf("Stop body = %v", e)
	}
}

func TestCodexHookBodies_StopRefreshesTitle(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "rollout.jsonl")
	writeCodexTranscript(t, transcript, "Add retries to the client")

	input := mustJSON(t, map[string]any{
		"hook_event_name": "Stop",
		"session_id":      "uuid",
		"transcript_path": transcript,
	})
	bodies := CodexHookBodies("Stop", input)
	if len(bodies) != 2 {
		t.Fatalf("Stop with transcript: want session+turn (2 bodies), got %d", len(bodies))
	}
	var sess map[string]any
	mustUnmarshal(t, bodies[0], &sess)
	if sess["op"] != "session" || sess["name"] != "Add retries to the client" {
		t.Errorf("first Stop body should refresh session title: %v", sess)
	}
	var turn map[string]string
	mustUnmarshal(t, bodies[1], &turn)
	if turn["phase"] != "end" {
		t.Errorf("second Stop body should end the turn: %v", turn)
	}
}

func TestCodexHookBodies_UnknownEvent(t *testing.T) {
	if bodies := CodexHookBodies("PreToolUse", []byte(`{}`)); bodies != nil {
		t.Errorf("unknown event should yield no bodies, got %v", bodies)
	}
}

func TestCodexHookArgs(t *testing.T) {
	const bin = "/usr/local/bin/gmux"
	out := codexHookArgs([]string{"codex", "resume", "abc"}, bin)

	// No global trust bypass flag is ever added.
	for _, a := range out {
		if strings.Contains(a, "bypass-hook-trust") {
			t.Fatalf("must not use the global trust-bypass flag: %v", out)
		}
	}
	// Binary token stays put; original tail preserved at the end.
	if out[0] != "codex" {
		t.Fatalf("binary token moved: %v", out)
	}
	if out[len(out)-2] != "resume" || out[len(out)-1] != "abc" {
		t.Errorf("original args not preserved at tail: %v", out)
	}
	// One -c hook override per subscribed event, each carrying the binary + event.
	for _, event := range codexHookEvents {
		found := false
		for i := 0; i < len(out)-1; i++ {
			if out[i] == "-c" && strings.HasPrefix(out[i+1], "hooks."+event+"=") &&
				strings.Contains(out[i+1], "__codex-hook "+event) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing -c override for event %s in %v", event, out)
		}
	}
	// Exactly one -c hooks.state override pre-trusting our hooks.
	stateCount := 0
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "-c" && strings.HasPrefix(out[i+1], "hooks.state=") {
			stateCount++
			for _, event := range codexHookEvents {
				if !strings.Contains(out[i+1], codexHookTrustKey(codexEventLabel(event))) {
					t.Errorf("hooks.state missing trust key for %s: %s", event, out[i+1])
				}
			}
		}
	}
	if stateCount != 1 {
		t.Errorf("want exactly 1 hooks.state override, got %d in %v", stateCount, out)
	}

	// Binary not at args[0] (e.g. `env codex`): injection goes after the binary.
	out = codexHookArgs([]string{"env", "codex"}, bin)
	if out[0] != "env" || out[1] != "codex" || out[2] != "-c" {
		t.Errorf("env codex: injection misplaced: %v", out)
	}

	// No codex binary token: unchanged.
	orig := []string{"bash", "-c", "echo hi"}
	if got := codexHookArgs(orig, bin); !reflect.DeepEqual(got, orig) {
		t.Errorf("non-codex args mutated: %v", got)
	}
}

// TestCodexHookOverrideIsValidTOML proves the injected `-c` values parse the way
// codex parses them: the hook-definition override deserializes into codex's
// MatcherGroup shape, and the hooks.state override deserializes into the
// per-hook trust map keyed exactly as codex's hook_key would compute it.
func TestCodexHookOverrideIsValidTOML(t *testing.T) {
	// A path with a space + an apostrophe, to exercise shell + TOML escaping.
	const bin = "/home/a b/it's/gmux"
	out := codexHookArgs([]string{"codex"}, bin)

	values := map[string]string{} // key path -> raw TOML value
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "-c" {
			k, v, _ := strings.Cut(out[i+1], "=")
			values[k] = v
		}
	}

	// Hook definition for SessionStart.
	def, ok := values["hooks.SessionStart"]
	if !ok {
		t.Fatal("no hooks.SessionStart override injected")
	}
	var defDoc struct {
		V []struct {
			Hooks []struct {
				Type    string `toml:"type"`
				Command string `toml:"command"`
				Timeout int    `toml:"timeout"`
			} `toml:"hooks"`
		} `toml:"v"`
	}
	if _, err := toml.Decode("v = "+def, &defDoc); err != nil {
		t.Fatalf("codex would reject hooks.SessionStart value %q: %v", def, err)
	}
	if len(defDoc.V) != 1 || len(defDoc.V[0].Hooks) != 1 {
		t.Fatalf("unexpected MatcherGroup shape: %+v", defDoc)
	}
	h := defDoc.V[0].Hooks[0]
	if h.Type != "command" || h.Timeout != 5 {
		t.Errorf("handler = %+v, want type=command timeout=5", h)
	}
	if !strings.Contains(h.Command, "__codex-hook SessionStart") ||
		!strings.Contains(h.Command, `'/home/a b/it'\''s/gmux'`) {
		t.Errorf("decoded command = %q", h.Command)
	}

	// Trust state: must parse into HookStateToml keyed exactly by hook_key, and
	// the decoded command's trusted_hash must equal what codexHookTrustedHash
	// produced for that same command.
	state, ok := values["hooks.state"]
	if !ok {
		t.Fatal("no hooks.state override injected")
	}
	var stateDoc struct {
		V map[string]struct {
			TrustedHash string `toml:"trusted_hash"`
		} `toml:"v"`
	}
	if _, err := toml.Decode("v = "+state, &stateDoc); err != nil {
		t.Fatalf("codex would reject hooks.state value %q: %v", state, err)
	}
	wantKey := codexHookTrustKey("session_start")
	entry, ok := stateDoc.V[wantKey]
	if !ok {
		t.Fatalf("hooks.state missing key %q; have %v", wantKey, stateDoc.V)
	}
	if entry.TrustedHash != codexHookTrustedHash(h.Command, "session_start") {
		t.Errorf("trusted_hash %q does not match the injected hook command", entry.TrustedHash)
	}
}

// TestCodexHookTrustedHash pins the canonical-JSON form codex hashes
// (version_for_toml): sorted keys, compact, no HTML escaping, no trailing
// newline. If this drifts, our hook is merely untrusted (skipped) — codex falls
// back to daemon attribution — but this guards the Go side from silent change.
func TestCodexHookTrustedHash(t *testing.T) {
	const cmd = `'/usr/local/bin/gmux' __codex-hook SessionStart`
	canonical := `{"event_name":"session_start","hooks":[{"async":false,"command":"` +
		cmd + `","timeout":5,"type":"command"}]}`
	sum := sha256.Sum256([]byte(canonical))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got := codexHookTrustedHash(cmd, "session_start"); got != want {
		t.Errorf("codexHookTrustedHash = %s, want %s (canonical %q)", got, want, canonical)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/gmux":   "'/usr/local/bin/gmux'",
		"/path/with space/gmux": "'/path/with space/gmux'",
		"/it's/weird/gmux":      `'/it'\''s/weird/gmux'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- helpers ---

func writeCodexTranscript(t *testing.T, path, firstUserText string) {
	t.Helper()
	meta := `{"type":"session_meta","payload":{"id":"uuid","timestamp":"2026-06-20T10:00:00Z","cwd":"/proj"}}`
	msg := mustJSON(t, map[string]any{
		"type": "response_item",
		"payload": map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": firstUserText},
			},
		},
	})
	if err := os.WriteFile(path, []byte(meta+"\n"+string(msg)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
}
