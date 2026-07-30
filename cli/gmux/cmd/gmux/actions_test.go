package main

import (
	"encoding/json"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestMatchSession covers the liberal reference grammar: full ID, slug, and
// unique prefixes of either. The full ID printed by `gmux ls` must resolve.
func TestMatchSession(t *testing.T) {
	sessions := []cliSession{
		{ID: "1va8lvdv", Slug: "fix-auth"},
		{ID: "14zknoqk", Slug: "fix-bug"},
		{ID: "1lp4cge2", Slug: "build-docs"},
	}

	cases := []struct {
		name   string
		ref    string
		wantID string
	}{
		{"full id", "1va8lvdv", "1va8lvdv"},
		{"exact slug", "fix-auth", "1va8lvdv"},
		{"unique id prefix", "1lp4", "1lp4cge2"},
		{"unique slug prefix", "build", "1lp4cge2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := matchSession(sessions, tc.ref)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tc.wantID)
			}
		})
	}
}

// TestMatchSessionAmbiguous asserts that ambiguous prefixes refuse to
// guess: killing the wrong session because a prefix happened to match
// two sessions would be actively harmful, much worse than a bad error
// message.
func TestMatchSessionAmbiguous(t *testing.T) {
	sessions := []cliSession{
		{ID: "1va8lvdv", Slug: "fix-auth"},
		{ID: "14zknoqk", Slug: "fix-bug"},
	}
	_, err := matchSession(sessions, "1")
	if err == nil {
		t.Fatal("expected ambiguous error")
	}
	// Both candidates must appear in the error so the user can
	// disambiguate by typing more characters.
	msg := err.Error()
	if !strings.Contains(msg, "1va8lvdv") || !strings.Contains(msg, "14zknoqk") {
		t.Errorf("error should list both candidates, got: %s", msg)
	}
}

// TestMatchSessionExactIDBeatsSlug pins the deterministic exact-match tie:
// an immutable ID wins over another session's exact slug.
func TestMatchSessionExactIDBeatsSlug(t *testing.T) {
	sessions := []cliSession{
		{ID: "1va8lvdv"},
		{ID: "wxyz5678", Slug: "1va8lvdv"},
	}
	got, err := matchSession(sessions, "1va8lvdv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "1va8lvdv" {
		t.Errorf("expected exact match to win, got %q", got.ID)
	}
}

// TestMatchSessionNoMatch is the "cold cache" path: the user typo'd or
// pointed at a session from another machine. Error, don't pick a
// random one.
func TestMatchSessionNoMatch(t *testing.T) {
	sessions := []cliSession{{ID: "1va8lvdv"}}
	if _, err := matchSession(sessions, "zzzz"); err == nil {
		t.Error("expected error for non-matching ref")
	}
	if _, err := matchSession(nil, "anything"); err == nil {
		t.Error("expected error when session list is empty")
	}
	if _, err := matchSession(sessions, ""); err == nil {
		t.Error("expected error for empty ref")
	}
}

// TestBuildSendBody pins the wire-level contract of `gmux send`: the
// bytes written to the PTY are exactly text/stdin then rendered keys —
// nothing implicit, and nothing derived from the session's adapter.
// Inline-text and stdin paths are both covered because they construct
// the body differently.
func TestBuildSendBody(t *testing.T) {
	noStdin := "\x00NIL" // sentinel: this case passes a nil stdin reader
	tests := []struct {
		name  string
		text  *string
		keys  []string
		stdin string // noStdin → nil reader (the tty / no-pipe case)
		want  string
	}{
		{
			name:  "text without keys sends verbatim, no submit",
			text:  stringPtr("hello"),
			stdin: noStdin,
			want:  "hello",
		},
		{
			name:  "text + Enter submits with trailing \\r",
			text:  stringPtr("hello"),
			keys:  []string{"Enter"},
			stdin: noStdin,
			want:  "hello\r",
		},
		{
			name:  "text + C-c appends the control byte",
			text:  stringPtr("hello"),
			keys:  []string{"C-c"},
			stdin: noStdin,
			want:  "hello\x03",
		},
		{
			name:  "keys only at a tty (nil stdin) sends just the keys",
			keys:  []string{"Escape", "Enter"},
			stdin: noStdin,
			want:  "\x1b\r",
		},
		{
			name:  "piped stdin, no keys, verbatim",
			stdin: "prompt body\nwith newline\n",
			want:  "prompt body\nwith newline\n",
		},
		{
			name:  "piped stdin composes with trailing Enter (no silent drop)",
			keys:  []string{"Enter"},
			stdin: "hi",
			want:  "hi\r",
		},
		{
			// Regression guard for the removed --steering/--follow-up
			// flags: a prompt with no key tokens is NOT submitted, for
			// any adapter. send is raw; semantic submission is the
			// agent layer's job (ADR 0027).
			name:  "prompt-shaped text is never auto-submitted",
			text:  stringPtr("also do X"),
			stdin: noStdin,
			want:  "also do X",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdin io.Reader
			if tc.stdin != noStdin {
				stdin = strings.NewReader(tc.stdin)
			}
			body := buildSendBody(tc.text, tc.keys, stdin)
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
		})
	}
}

func stringPtr(s string) *string { return &s }

// TestMatchSessionStrictLocalDefault locks in the rule that drove
// the new addressing design: with no --host and no @suffix, peer
// sessions are invisible to the lookup. A user who has only a peer
// session with id "1va8lvdv" must not have `gmux --kill 1va8lvdv`
// silently kill it; they have to opt in via @peer or --host.
func TestMatchSessionStrictLocalDefault(t *testing.T) {
	sessions := []cliSession{
		{ID: "1va8lvdv", Peer: "konyvtar"},
	}
	_, err := matchSession(sessions, "1va8lvdv")
	if err == nil {
		t.Fatal("strict-local lookup should not see a peer-only session")
	}
}

// TestMatchSessionFriendlyHintForPeerOnlyMatch is the UX safety net
// for the strict-local rule: when the ref only matches a peer
// session, the error must point the user at the qualified form
// rather than reading like "this session doesn't exist." Otherwise
// the strict default feels like a regression.
func TestMatchSessionFriendlyHintForPeerOnlyMatch(t *testing.T) {
	sessions := []cliSession{
		{ID: "1va8lvdv", Peer: "konyvtar"},
	}
	_, err := matchSession(sessions, "1va8lvdv")
	if err == nil {
		t.Fatal("expected error for peer-only ID without --host")
	}
	msg := err.Error()
	if !strings.Contains(msg, "1va8lvdv@konyvtar") {
		t.Errorf("error should suggest qualified form, got: %s", msg)
	}
}

// TestMatchSessionAtSuffixRoutes is the canonical address form: an
// `id@host` ref resolves to the session on that host without needing
// the --host flag. Any divergence here would break the design's
// claim that copy-paste from `--list --all` works directly with
// action subcommands.
func TestMatchSessionAtSuffixRoutes(t *testing.T) {
	sessions := []cliSession{
		{ID: "1va8lvdv"},                   // local
		{ID: "1va8lvdv", Peer: "konyvtar"}, // namespaced collision
		{ID: "1lp4cge2", Peer: "bespin"},
	}
	got, err := matchSession(sessions, "1va8lvdv@konyvtar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Peer != "konyvtar" {
		t.Errorf("expected konyvtar session, got peer=%q", got.Peer)
	}
}

// TestMatchSessionEmptyHostSuffixRejected covers the typo case where a
// user types `id@` with no host after. Silently scoping that to local
// (the old behavior) gave the user no signal that the @host they
// intended to type was missing, and they would address the wrong
// session if a local one happened to match.
func TestMatchSessionEmptyHostSuffixRejected(t *testing.T) {
	sessions := []cliSession{
		{ID: "1va8lvdv"},                   // local
		{ID: "1va8lvdv", Peer: "konyvtar"}, // peer
	}
	_, err := matchSession(sessions, "1va8lvdv@")
	if err == nil {
		t.Fatal("expected error for trailing @ with empty host suffix")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty host, got: %s", err.Error())
	}
}

// TestMatchSessionMultiplePeerMatchesGetCandidateList exercises the
// other half of the friendly-miss UX: when a prefix matches sessions
// on more than one peer, listing the qualified candidates is the
// only actionable answer. Picking one to suggest would silently
// favor an arbitrary peer; saying "not found" would hide that peer
// sessions exist; saying "ambiguous" without candidates leaves the
// user typing more characters and hoping.
//
// Realistic shape: full session IDs are globally unique, but a short
// prefix the user typed (or copy-pasted before fully selecting the
// id) can match multiple sessions across peers.
func TestMatchSessionMultiplePeerMatchesGetCandidateList(t *testing.T) {
	sessions := []cliSession{
		{ID: "1va8lvdv", Peer: "konyvtar"},
		{ID: "15g979sl", Peer: "bespin"},
	}
	_, err := matchSession(sessions, "1")
	if err == nil {
		t.Fatal("expected error for prefix matching multiple peer sessions")
	}
	msg := err.Error()
	// Both qualified forms must appear; the user uses the message to
	// pick the right one and retypes.
	if !strings.Contains(msg, "1va8lvdv@konyvtar") || !strings.Contains(msg, "15g979sl@bespin") {
		t.Errorf("error should list both qualified candidates, got: %s", msg)
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "hello world\n", "hello world\n"},
		{"CSI color codes removed", "\x1b[31mred\x1b[0m text", "red text"},
		{"cursor-move CSI removed", "a\x1b[2Kb\x1b[1;5Hc", "abc"},
		{"OSC title (BEL-terminated) removed", "\x1b]0;my title\x07done", "done"},
		{"OSC (ST-terminated) removed", "\x1b]8;;http://x\x1b\\link", "link"},
		{"CRLF normalized to LF", "line1\r\nline2\r\n", "line1\nline2\n"},
		{"UTF-8 multibyte preserved", "café — π ✓", "café — π ✓"},
		{"lone ESC at end does not panic", "trailing\x1b", "trailing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(stripANSI([]byte(tc.in))); got != tc.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsNoMatchError(t *testing.T) {
	// The specific "no session matches" error from matchSession is retryable.
	sessions := []cliSession{{ID: "1va8lvdv"}}
	_, err := matchSession(sessions, "zzzz")
	if err == nil {
		t.Fatal("expected error")
	}
	if !isNoMatchError(err) {
		t.Errorf("expected isNoMatchError=true for %q", err)
	}

	// Ambiguous errors are NOT retryable.
	sessions = []cliSession{
		{ID: "1va8lvdv"},
		{ID: "14zknoqk"},
	}
	_, err = matchSession(sessions, "1")
	if err == nil {
		t.Fatal("expected error")
	}
	if isNoMatchError(err) {
		t.Errorf("ambiguous error should not be retryable: %q", err)
	}

	// Empty ref errors are NOT retryable.
	_, err = matchSession(nil, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if isNoMatchError(err) {
		t.Errorf("empty ref error should not be retryable: %q", err)
	}
}

// TestListJSONSchemaIsStable pins the `gmux ls --json` key set. ADR 0009
// decision 13b promised a "stable schema" and then described one the code never
// emitted (`kind` for what is really `adapter`, plus an `idle` field that has
// never existed, since idleness is a turn property read through `gmux wait`,
// not a property of a list row). Scripts are written against this shape, so a
// rename or a dropped field should fail here rather than in someone's pipeline.
func TestListJSONSchemaIsStable(t *testing.T) {
	exit := 0
	full := cliSession{
		ID: "1va8lvdv", Peer: "laptop", Cwd: "/home/mg/dev/gmux",
		Adapter: "pi", Alive: true, Pid: 4242, Title: "fix auth bug",
		Slug: "fix-auth-bug", ParentSessionID: "1u0xpj5g",
		SocketPath: "/run/gmux/1va8lvdv.sock",
		Command:    []string{"pi", "--model", "sonnet"},
		StartedAt:  "2026-07-27T10:00:00Z", ExitedAt: "2026-07-27T10:05:00Z",
		ExitCode: &exit,
	}
	wantAll := []string{
		"id", "peer", "cwd", "adapter", "alive", "pid", "title", "slug",
		"parent_session_id", "socket_path", "command", "started_at",
		"exited_at", "exit_code",
	}
	got := jsonKeys(t, full)
	for _, k := range wantAll {
		if _, ok := got[k]; !ok {
			t.Errorf("populated session omits documented key %q; keys: %v", k, sortedKeys(got))
		}
	}
	for k := range got {
		if !slices.Contains(wantAll, k) {
			t.Errorf("undocumented key %q in ls --json; document it in ADR 0009 13b and reference/cli.md", k)
		}
	}

	// A minimal session must still answer the three questions every script
	// asks: which session, what is running, is it still running.
	bare := jsonKeys(t, cliSession{ID: "1va8lvdv"})
	for _, k := range []string{"id", "adapter", "alive"} {
		if _, ok := bare[k]; !ok {
			t.Errorf("zero-value session omits always-present key %q; keys: %v", k, sortedKeys(bare))
		}
	}
	for _, k := range []string{"peer", "cwd", "pid", "title", "slug", "parent_session_id", "socket_path", "command", "started_at", "exited_at", "exit_code"} {
		if _, ok := bare[k]; ok {
			t.Errorf("key %q should be omitempty but appears on a zero-value session", k)
		}
	}
	if _, ok := got["kind"]; ok {
		t.Error("`kind` is the pre-adapter wire name and must not come back (UBIQUITOUS_LANGUAGE.md)")
	}
	if _, ok := got["idle"]; ok {
		t.Error("`idle` must not be a list field: idleness is a turn property, read via `gmux wait`")
	}
}

func jsonKeys(t *testing.T, s cliSession) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal cliSession: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal cliSession: %v", err)
	}
	return m
}

func sortedKeys(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
