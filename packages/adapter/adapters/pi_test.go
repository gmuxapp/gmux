package adapters

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// TestPiSubcommandsMatchHelp is a drift-guard against real pi: it parses the
// Commands block of `pi --help` and asserts every advertised command is in our
// passthrough set. Forward-compatible entries may remain in the set while CI's
// installed pi is older than the release that introduced them.
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
	// `auth` first appears after pi 0.82.1. CI intentionally exercises multiple
	// pi versions, so the production set is a forward-compatible superset rather
	// than requiring every older help page to advertise every known command.
	forwardCompatible := map[string]bool{"auth": true}
	for verb := range piSubcommands {
		if !got[verb] && !forwardCompatible[verb] {
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

// --- Env ---

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

// TestPiEncodeAction pins the exact terminal encodings pi's semantic
// turn-control actions map to. Changing these bytes changes what the
// agent receives; keep in sync with the adapter docs.
//
// Why not the obvious legacy encodings: pi-tui tries Kitty CSI-u
// parsing unconditionally, so CSI-u works under either negotiated
// keyboard protocol, whereas ESC CR (alt+enter) is read as shift+enter
// — a newline, no submit — whenever pi negotiated the Kitty protocol,
// and a lone ESC byte is the prefix of every escape sequence, so it is
// only unambiguous when nothing follows it in the same read.
func TestPiEncodeAction(t *testing.T) {
	pi := NewPi()
	cases := []struct {
		name   string
		action adapter.AgentAction
		want   string
	}{
		{"send is Enter", adapter.ActionSend, "\r"},
		{"send-after-turn is Kitty CSI-u alt+enter", adapter.ActionSendAfterTurn, "\x1b[13;3u"},
		{"interrupt is Kitty CSI-u escape", adapter.ActionInterrupt, "\x1b[27u"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pi.EncodeAction(tc.action)
			if !ok || got != tc.want {
				t.Errorf("EncodeAction(%d) = %q, %v; want %q, true", tc.action, got, ok, tc.want)
			}
		})
	}
}

// TestPiEncodeActionUnknown pins that an out-of-vocabulary action is
// rejected rather than silently encoded as something plausible: a
// future action added to the enum must fail loudly here until pi
// actually maps it.
func TestPiEncodeActionUnknown(t *testing.T) {
	pi := NewPi()
	for _, action := range []adapter.AgentAction{adapter.ActionInterrupt + 1, -1, 99} {
		if seq, ok := pi.EncodeAction(action); ok || seq != "" {
			t.Errorf("EncodeAction(%d) = %q, %v; want \"\", false", action, seq, ok)
		}
	}
}

// TestPiActionReadyTimeout pins the adapter-owned readiness bound. pi
// is a Node program: cold start is seconds, so a short timeout would
// spuriously fail freshly launched or resumed sessions.
func TestPiActionReadyTimeout(t *testing.T) {
	if got := NewPi().ActionReadyTimeout(); got != 10*time.Second {
		t.Errorf("ActionReadyTimeout() = %v, want 10s", got)
	}
}

// --- Capability interface checks ---

func TestPiImplementsCapabilities(t *testing.T) {
	var a adapter.Adapter = NewPi()
	if _, ok := a.(adapter.Launchable); !ok {
		t.Fatal("should implement Launchable")
	}
	if _, ok := a.(adapter.ConversationDescriber); !ok {
		t.Fatal("should implement ConversationDescriber")
	}
	if _, ok := a.(adapter.Resumer); !ok {
		t.Fatal("should implement Resumer")
	}
	if _, ok := a.(adapter.AgentActionEncoder); !ok {
		t.Fatal("should implement AgentActionEncoder")
	}
}

// --- Conversation storage ---

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

// The ref/LastActivity contract (ADR 0022): DescribeConversation must echo
// the ref it was given and report freshness itself — file-backed adapters
// answer with the transcript's mtime, so consumers (activity reseeding,
// search recency) never stat anything.
func TestDescribeConversationRefAndLastActivity(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc-123","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
	)
	mtime := time.Date(2026, 3, 16, 9, 30, 0, 0, time.UTC)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	info, err := NewPi().DescribeConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Ref != path {
		t.Errorf("Ref = %q, want the ref echoed back (%q)", info.Ref, path)
	}
	if !info.LastActivity.Equal(mtime) {
		t.Errorf("LastActivity = %v, want transcript mtime %v", info.LastActivity, mtime)
	}
}

// OpenConversation is the content seam for derived consumers (fulltext
// search): it must stream the raw transcript bytes for a ref.
func TestOpenConversationStreamsTranscript(t *testing.T) {
	line := `{"type":"session","version":3,"id":"abc-123","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`
	path := writeTempJSONL(t, line)
	rc, err := NewPi().OpenConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != line+"\n" {
		t.Errorf("OpenConversation content = %q, want the raw transcript", got)
	}
}

func TestDescribeConversationFirstUserMessage(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc-123","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"model_change","id":"m1","timestamp":"2026-03-15T10:00:00Z"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"Fix the auth bug in login.go"}]}}`,
		`{"type":"message","id":"a1","timestamp":"2026-03-15T10:01:05Z","message":{"role":"assistant","content":[{"type":"text","text":"I'll fix that for you."}]}}`,
	)
	info, err := NewPi().DescribeConversation(path)
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
	if len(info.AncestorIDs) != 0 {
		t.Errorf("expected no ancestors for in-place pi resume, got %v", info.AncestorIDs)
	}
}

func TestDescribeConversationNameOverrides(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"Fix the auth bug"}]}}`,
		`{"type":"session_info","name":"  Auth refactor  "}`,
	)
	info, _ := NewPi().DescribeConversation(path)
	if info.Title != "Auth refactor" {
		t.Errorf("expected session_info name, got %q", info.Title)
	}
	// Slug derives from final title (session_info.name overrides user msg).
	if info.Slug != "auth-refactor" {
		t.Errorf("expected slug from session name, got %q", info.Slug)
	}
}

func TestDescribeConversationNoMessages(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
	)
	info, _ := NewPi().DescribeConversation(path)
	if info.Title != "" {
		t.Errorf("expected empty title for a session with no messages, got %q", info.Title)
	}
}

func TestDescribeConversationLongTitleTruncated(t *testing.T) {
	long := "Please help me with this very long request that goes on and on about many different things and really should be truncated for the sidebar"
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"`+long+`"}]}}`,
	)
	info, _ := NewPi().DescribeConversation(path)
	if len(info.Title) > 85 {
		t.Errorf("title too long: %q", info.Title)
	}
}

func TestDescribeConversationStringContent(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":"Help me debug this"}}`,
	)
	info, _ := NewPi().DescribeConversation(path)
	if info.Title != "Help me debug this" {
		t.Errorf("expected string content as title, got %q", info.Title)
	}
}

// --- Resumer ---

func TestResumeCommand(t *testing.T) {
	cmd := NewPi().ResumeCommand(&adapter.ConversationInfo{
		Ref:          "/tmp/test.jsonl",
		MessageCount: 1,
	})
	want := []string{"pi", "--session", "/tmp/test.jsonl", "-c"}
	if !slices.Equal(cmd, want) {
		t.Errorf("resume command = %v, want %v", cmd, want)
	}
}

func TestResumeCommandResumability(t *testing.T) {
	p := NewPi()
	valid := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
	)
	info, err := p.DescribeConversation(valid)
	if err != nil {
		t.Fatalf("DescribeConversation: %v", err)
	}
	if cmd := p.ResumeCommand(info); !slices.Equal(cmd, []string{"pi", "--session", valid, "-c"}) {
		t.Fatalf("should resume by its own ref, got %v", cmd)
	}

	empty := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
	)
	info, err = p.DescribeConversation(empty)
	if err != nil {
		t.Fatalf("DescribeConversation: %v", err)
	}
	if cmd := p.ResumeCommand(info); cmd != nil {
		t.Fatalf("empty session should not be resumable, got %v", cmd)
	}
	if cmd := p.ResumeCommand(nil); cmd != nil {
		t.Fatalf("nil info should not be resumable, got %v", cmd)
	}
}

// --- Helpers ---

func TestConversationDirEncoding(t *testing.T) {
	dir := NewPi().ConversationDir("/home/mg/dev/gmux")
	if base := filepath.Base(dir); base != "--home-mg-dev-gmux--" {
		t.Errorf("expected --home-mg-dev-gmux--, got %s", base)
	}
}

func TestConversationRootDirRespectsEnvVar(t *testing.T) {
	custom := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", custom)
	root := NewPi().ConversationRootDir()
	want := filepath.Join(custom, "sessions")
	if root != want {
		t.Errorf("expected %s, got %s", want, root)
	}
}

func TestConversationRootDirDefaultWithoutEnvVar(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")
	root := NewPi().ConversationRootDir()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".pi", "agent", "sessions")
	if root != want {
		t.Errorf("expected %s, got %s", want, root)
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

// TestPiConversationGone verifies the adapter anchors deletion detection
// on its own ConversationRootDir: a missing file under a live root reads as
// deleted, while a missing root (unreachable storage) is undeterminable.
func TestPiConversationGone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	p := NewPi()
	root := p.ConversationRootDir() // <dir>/sessions
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	// File deleted, storage present → gone.
	if gone, ok := p.ConversationGone(filepath.Join(root, "x", "conv.jsonl")); !gone || !ok {
		t.Errorf("deleted-under-live-root: got (gone=%v ok=%v), want (true true)", gone, ok)
	}

	// File present → not gone.
	live := filepath.Join(root, "live.jsonl")
	if err := os.WriteFile(live, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if gone, ok := p.ConversationGone(live); gone || !ok {
		t.Errorf("present: got (gone=%v ok=%v), want (false true)", gone, ok)
	}

	// Storage root absent → undeterminable, never a false deletion.
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(dir, "unmounted"))
	if gone, ok := p.ConversationGone(filepath.Join(dir, "unmounted", "sessions", "c.jsonl")); gone || ok {
		t.Errorf("absent-root: got (gone=%v ok=%v), want (false false)", gone, ok)
	}
}
