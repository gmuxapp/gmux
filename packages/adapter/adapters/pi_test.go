package adapters

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// TestPiSubcommandsMatchHelp is a drift-guard against real pi: it parses the
// Commands block of `pi --help` and asserts it exactly matches piSubcommands.
// This is the detector for the highest-risk drift — pi adding or renaming a
// subcommand — which IsPassthrough would otherwise miss, silently demoting
// `gmux -- pi <newverb>` to a chat prompt. CI runs it against pi@latest (PRs +
// nightly) so a pi release that changes the verb set fails loudly and demands a
// fast-tracked fix to piSubcommands. Skips when pi is absent or under -short.
func TestPiSubcommandsMatchHelp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-pi drift guard in -short mode")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi binary not on PATH; skipping drift guard")
	}

	cmd := exec.Command("pi", "--help")
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pi --help: %v\n%s", err, out)
	}

	got := parseHelpSubcommands(string(out))
	if len(got) == 0 {
		t.Fatalf("found no subcommands in `pi --help` — has the Commands block format changed?\n%s", out)
	}
	for verb := range got {
		if !piSubcommands[verb] {
			t.Errorf("pi added subcommand %q not in piSubcommands — `gmux -- pi %s` would be demoted to a prompt; add it (pi.go)", verb, verb)
		}
	}
	for verb := range piSubcommands {
		if !got[verb] {
			t.Errorf("piSubcommands has %q but `pi --help` no longer lists it — remove it (pi.go)", verb)
		}
	}
}

// parseHelpSubcommands extracts the verbs from the `Commands:` block of
// `pi --help` (indented `  pi <verb> ...` lines; the `pi <command> --help`
// hint line doesn't match and is skipped).
func parseHelpSubcommands(help string) map[string]bool {
	verb := regexp.MustCompile(`^\s+pi ([a-z][a-z-]*)\b`)
	got := map[string]bool{}
	inCommands := false
	for _, line := range strings.Split(help, "\n") {
		if strings.HasPrefix(line, "Commands:") {
			inCommands = true
			continue
		}
		if !inCommands {
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			break // dedented line → next section (Options:)
		}
		if m := verb.FindStringSubmatch(line); m != nil {
			got[m[1]] = true
		}
	}
	return got
}

// --- Matching ---

func TestPiName(t *testing.T) {
	if NewPi().Name() != "pi" {
		t.Fatal("expected 'pi'")
	}
}

func TestPiMatchDirect(t *testing.T) {
	p := NewPi()
	if !p.Match([]string{"pi"}) {
		t.Fatal("should match 'pi'")
	}
	if !p.Match([]string{"pi-coding-agent"}) {
		t.Fatal("should match 'pi-coding-agent'")
	}
}

func TestPiMatchWrapped(t *testing.T) {
	p := NewPi()
	if !p.Match([]string{"npx", "pi"}) {
		t.Fatal("should match 'npx pi'")
	}
	if !p.Match([]string{"env", "pi", "--flag"}) {
		t.Fatal("should match 'env pi --flag'")
	}
	if !p.Match([]string{"/home/user/.local/bin/pi"}) {
		t.Fatal("should match full path")
	}
}

func TestPiMatchStopsAtDoubleDash(t *testing.T) {
	if NewPi().Match([]string{"echo", "--", "pi"}) {
		t.Fatal("should not match 'pi' after '--'")
	}
}

func TestPiIsPassthrough(t *testing.T) {
	p := NewPi()
	passthrough := [][]string{
		{"pi", "update"},
		{"pi", "update", "self"},
		{"pi", "list"},
		{"pi", "config"},
		{"pi", "install", "foo"},
		{"pi", "remove", "foo"},
		{"pi", "uninstall", "foo"},
		{"/home/user/.local/bin/pi", "update"}, // path-qualified binary
		{"pi", "--help"},                       // info flags short-circuit pi
		{"pi", "-h"},
		{"pi", "--version"},
		{"pi", "--name", "x", "--help"}, // info flag anywhere in top-level args
	}
	for _, args := range passthrough {
		if !p.IsPassthrough(args) {
			t.Errorf("expected passthrough for %v", args)
		}
	}
	sessions := [][]string{
		{"pi"},                         // bare interactive
		{"pi", "--name", "x"},          // named session
		{"pi", "-c"},                   // continue
		{"pi", "-r"},                   // resume picker
		{"pi", "--session", "abc"},     // resume by id
		{"pi", "update is broken"},     // a chat message that starts with a verb
		{"pi", "--name", "list"},       // "list" as a flag value, not argv[1]
		{"echo", "--", "pi", "update"}, // not pi at all
	}
	for _, args := range sessions {
		if p.IsPassthrough(args) {
			t.Errorf("expected session (not passthrough) for %v", args)
		}
	}
}

func TestPiNoMatchOther(t *testing.T) {
	p := NewPi()
	if p.Match([]string{"pytest"}) {
		t.Fatal("should not match pytest")
	}
	if p.Match([]string{"pipeline"}) {
		t.Fatal("should not match 'pipeline'")
	}
}

// --- Env / Monitor ---

func TestPiEnvNil(t *testing.T) {
	if env := NewPi().Env(adapter.EnvContext{}); env != nil {
		t.Fatalf("expected nil, got %v", env)
	}
}

func TestPiDiscover(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: depends on pi being installed")
	}
	// LookPath-based: result depends on the test machine.
	_ = NewPi().Discover()
}

func TestPiMonitorNoOp(t *testing.T) {
	// Pi Monitor is a no-op — status is driven by FileMonitor.
	pi := NewPi()
	if pi.Monitor([]byte("⠋ Working...")) != nil {
		t.Fatal("should return nil (file-driven, not PTY)")
	}
	if pi.Monitor([]byte("some output")) != nil {
		t.Fatal("should return nil")
	}
}

// --- Capability interface checks ---

func TestPiImplementsCapabilities(t *testing.T) {
	var a adapter.Adapter = NewPi()
	if _, ok := a.(adapter.Launchable); !ok {
		t.Fatal("should implement Launchable")
	}
	if _, ok := a.(adapter.SessionFiler); !ok {
		t.Fatal("should implement SessionFiler")
	}
	if _, ok := a.(adapter.Resumer); !ok {
		t.Fatal("should implement Resumer")
	}
}

// --- SessionFiler ---

func writeTempJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test-session.jsonl")
	var content string
	for _, l := range lines {
		content += l + "\n"
	}
	os.WriteFile(path, []byte(content), 0644)
	return path
}

func TestParseSessionFileFirstUserMessage(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc-123","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"model_change","id":"m1","timestamp":"2026-03-15T10:00:00Z"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"Fix the auth bug in login.go"}]}}`,
		`{"type":"message","id":"a1","timestamp":"2026-03-15T10:01:05Z","message":{"role":"assistant","content":[{"type":"text","text":"I'll fix that for you."}]}}`,
	)
	info, err := NewPi().ParseSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "abc-123" {
		t.Errorf("expected id abc-123, got %s", info.ID)
	}
	if info.Title != "Fix the auth bug in login.go" {
		t.Errorf("expected first user msg as title, got %q", info.Title)
	}
	if info.MessageCount != 2 {
		t.Errorf("expected 2 messages, got %d", info.MessageCount)
	}
	if info.Slug != "fix-the-auth-bug-in-login-go" {
		t.Errorf("expected slug from title, got %q", info.Slug)
	}
}

func TestParseSessionFileNameOverrides(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"Fix the auth bug"}]}}`,
		`{"type":"session_info","name":"  Auth refactor  "}`,
	)
	info, _ := NewPi().ParseSessionFile(path)
	if info.Title != "Auth refactor" {
		t.Errorf("expected session_info name, got %q", info.Title)
	}
	// Slug derives from final title (session_info.name overrides user msg).
	if info.Slug != "auth-refactor" {
		t.Errorf("expected slug from session name, got %q", info.Slug)
	}
}

func TestParseSessionFileNoMessages(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
	)
	info, _ := NewPi().ParseSessionFile(path)
	if info.Title != "(new)" {
		t.Errorf("expected '(new)', got %q", info.Title)
	}
}

func TestParseSessionFileLongTitleTruncated(t *testing.T) {
	long := "Please help me with this very long request that goes on and on about many different things and really should be truncated for the sidebar"
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"`+long+`"}]}}`,
	)
	info, _ := NewPi().ParseSessionFile(path)
	if len(info.Title) > 85 {
		t.Errorf("title too long: %q", info.Title)
	}
}

func TestParseSessionFileStringContent(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":"Help me debug this"}}`,
	)
	info, _ := NewPi().ParseSessionFile(path)
	if info.Title != "Help me debug this" {
		t.Errorf("expected string content as title, got %q", info.Title)
	}
}

// --- FileMonitor ---

// --- Resumer ---

func TestResumeCommand(t *testing.T) {
	cmd := NewPi().ResumeCommand(&adapter.SessionFileInfo{
		FilePath: "/tmp/test.jsonl",
	})
	if len(cmd) != 4 || cmd[0] != "pi" || cmd[1] != "--session" || cmd[3] != "-c" {
		t.Errorf("unexpected resume command: %v", cmd)
	}
}

func TestCanResume(t *testing.T) {
	valid := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
	)
	if !NewPi().CanResume(valid) {
		t.Fatal("should be resumable")
	}

	empty := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
	)
	if NewPi().CanResume(empty) {
		t.Fatal("empty session should not be resumable")
	}
}

// --- Helpers ---

func TestSessionDirEncoding(t *testing.T) {
	dir := NewPi().SessionDir("/home/mg/dev/gmux")
	if base := filepath.Base(dir); base != "--home-mg-dev-gmux--" {
		t.Errorf("expected --home-mg-dev-gmux--, got %s", base)
	}
}

func TestSessionRootDirRespectsEnvVar(t *testing.T) {
	custom := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", custom)
	root := NewPi().SessionRootDir()
	want := filepath.Join(custom, "sessions")
	if root != want {
		t.Errorf("expected %s, got %s", want, root)
	}
}

func TestSessionRootDirDefaultWithoutEnvVar(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")
	root := NewPi().SessionRootDir()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".pi", "agent", "sessions")
	if root != want {
		t.Errorf("expected %s, got %s", want, root)
	}
}

func TestListSessionFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "b.jsonl"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("nope"), 0644)
	if len(ListSessionFiles(dir)) != 2 {
		t.Fatal("expected 2 jsonl files")
	}
}

func TestPiExtendCommand(t *testing.T) {
	p := NewPi()
	const ext = "/cache/pi-ext.mjs"
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"direct", []string{"pi", "--name", "x"}, []string{"pi", "-e", ext, "--name", "x"}},
		{"bare", []string{"pi"}, []string{"pi", "-e", ext}},
		// The binary is not args[0]: -e must go after pi, not the wrapper, or the
		// wrapper rejects it (the env/npx-pi launch-failure bug).
		{"env wrapper", []string{"env", "pi", "--name", "x"}, []string{"env", "pi", "-e", ext, "--name", "x"}},
		{"npx wrapper", []string{"npx", "pi"}, []string{"npx", "pi", "-e", ext}},
		{"path-qualified", []string{"/usr/bin/pi", "-c"}, []string{"/usr/bin/pi", "-e", ext, "-c"}},
		// No pi token before --: inject nothing.
		{"no pi", []string{"echo", "hi"}, []string{"echo", "hi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.ExtendCommand(tc.args, ext); !eq(got, tc.want) {
				t.Errorf("ExtendCommand(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
