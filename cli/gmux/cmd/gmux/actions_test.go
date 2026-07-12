package main

import (
	"io"
	"strings"
	"testing"
)

// TestMatchSession covers the reference-resolution rules the CLI
// documents: short form (as shown by --list), full ID, slug, and
// unique prefixes of any of those. These cases double as the
// compatibility contract between --list's output and the other
// management flags — if --list prints "abcd1234", `--kill abcd1234`
// must resolve it.
func TestMatchSession(t *testing.T) {
	sessions := []cliSession{
		{ID: "sess-abcd1234", Slug: "fix-auth"},
		{ID: "sess-abcd5678", Slug: "fix-bug"},
		{ID: "sess-ef019283", Slug: "build-docs"},
	}

	cases := []struct {
		name   string
		ref    string
		wantID string
	}{
		{"full id", "sess-abcd1234", "sess-abcd1234"},
		{"short form as shown by --list", "abcd1234", "sess-abcd1234"},
		{"exact slug", "fix-auth", "sess-abcd1234"},
		{"unique short-form prefix", "ef01", "sess-ef019283"},
		{"unique slug prefix", "build", "sess-ef019283"},
		{"unique full-id prefix", "sess-ef", "sess-ef019283"},
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
		{ID: "sess-abcd1234", Slug: "fix-auth"},
		{ID: "sess-abcd5678", Slug: "fix-bug"},
	}
	_, err := matchSession(sessions, "abcd")
	if err == nil {
		t.Fatal("expected ambiguous error")
	}
	// Both candidates must appear in the error so the user can
	// disambiguate by typing more characters.
	msg := err.Error()
	if !strings.Contains(msg, "abcd1234") || !strings.Contains(msg, "abcd5678") {
		t.Errorf("error should list both candidates, got: %s", msg)
	}
}

// TestMatchSessionExactBeatsPrefix covers the corner case where the
// user's ref is itself a valid session short id AND a prefix of
// another: the exact match must win, otherwise the unambiguous case
// would report ambiguity.
func TestMatchSessionExactBeatsPrefix(t *testing.T) {
	sessions := []cliSession{
		{ID: "sess-abcd"},     // exact match for short form "abcd"
		{ID: "sess-abcdef01"}, // also starts with "abcd"
	}
	got, err := matchSession(sessions, "abcd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "sess-abcd" {
		t.Errorf("expected exact match to win, got %q", got.ID)
	}
}

// TestMatchSessionNoMatch is the "cold cache" path: the user typo'd or
// pointed at a session from another machine. Error, don't pick a
// random one.
func TestMatchSessionNoMatch(t *testing.T) {
	sessions := []cliSession{{ID: "sess-abcd1234"}}
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

// TestShortID covers the conversion between gmuxd's full session IDs
// and the display form shown by --list, which is what users type back
// into --attach / --kill / --tail.
func TestShortID(t *testing.T) {
	cases := map[string]string{
		"sess-abcd1234": "abcd1234", // normal case
		"sess-ab":       "ab",       // unusually short (shouldn't happen, but don't crash)
		"abcd1234":      "abcd1234", // already short — idempotent
		"":              "",         // defensive
	}
	for in, want := range cases {
		if got := shortID(in); got != want {
			t.Errorf("shortID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildSendBody pins the wire-level contract of `gmux send`: the
// bytes written to the PTY are exactly text/stdin, then rendered keys,
// then the adapter submit sequence — nothing implicit. Inline-text and
// stdin paths are both covered because they construct the body
// differently, and the submit sequence has to compose with both.
func TestBuildSendBody(t *testing.T) {
	noStdin := "\x00NIL" // sentinel: this case passes a nil stdin reader
	tests := []struct {
		name   string
		text   *string
		keys   []string
		stdin  string // noStdin → nil reader (the tty / no-pipe case)
		submit string // adapter submit sequence (--follow-up/--steering)
		want   string
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
			name:   "text + follow-up submit seq (pi Alt+Enter)",
			text:   stringPtr("also do X"),
			stdin:  noStdin,
			submit: "\x1b[13;3u",
			want:   "also do X\x1b[13;3u",
		},
		{
			name:   "piped stdin + steering submit seq",
			stdin:  "course-correct",
			submit: "\r",
			want:   "course-correct\r",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdin io.Reader
			if tc.stdin != noStdin {
				stdin = strings.NewReader(tc.stdin)
			}
			body := buildSendBody(tc.text, tc.keys, stdin, tc.submit)
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

// TestSubmitSeqFor pins the adapter-name → submit-bytes mapping that
// `gmux send --follow-up/--steering` relies on: pi distinguishes the
// two modes (Enter vs Alt+Enter), while every other adapter — shells,
// agents whose Enter both submits and queues, and adapter names this
// build doesn't know (e.g. a peer session created by a newer gmux) —
// falls back to the universal Enter for both.
func TestSubmitSeqFor(t *testing.T) {
	cases := []struct {
		adapter string
		mode    string
		want    string
	}{
		{"pi", "steering", "\r"},
		{"pi", "follow-up", "\x1b[13;3u"}, // Alt+Enter, Kitty CSI-u encoding
		{"shell", "steering", "\r"},
		{"shell", "follow-up", "\r"},
		{"claude", "follow-up", "\r"},
		{"codex", "steering", "\r"},
		{"some-future-adapter", "follow-up", "\r"},
	}
	for _, tc := range cases {
		t.Run(tc.adapter+"/"+tc.mode, func(t *testing.T) {
			got, ok := submitSeqFor(tc.adapter, tc.mode)
			if !ok {
				t.Fatalf("submitSeqFor(%q, %q) not ok", tc.adapter, tc.mode)
			}
			if got != tc.want {
				t.Errorf("submitSeqFor(%q, %q) = %q, want %q", tc.adapter, tc.mode, got, tc.want)
			}
		})
	}
}

// TestMatchSessionStrictLocalDefault locks in the rule that drove
// the new addressing design: with no --host and no @suffix, peer
// sessions are invisible to the lookup. A user who has only a peer
// session with id "abcd1234" must not have `gmux --kill abcd1234`
// silently kill it; they have to opt in via @peer or --host.
func TestMatchSessionStrictLocalDefault(t *testing.T) {
	sessions := []cliSession{
		{ID: "sess-abcd1234", Peer: "konyvtar"},
	}
	_, err := matchSession(sessions, "abcd1234")
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
		{ID: "sess-abcd1234", Peer: "konyvtar"},
	}
	_, err := matchSession(sessions, "abcd1234")
	if err == nil {
		t.Fatal("expected error for peer-only short id without --host")
	}
	msg := err.Error()
	if !strings.Contains(msg, "abcd1234@konyvtar") {
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
		{ID: "sess-abcd1234"},                   // local
		{ID: "sess-abcd1234", Peer: "konyvtar"}, // namespaced collision
		{ID: "sess-ef019283", Peer: "bespin"},
	}
	got, err := matchSession(sessions, "abcd1234@konyvtar")
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
		{ID: "sess-abcd1234"},                   // local
		{ID: "sess-abcd1234", Peer: "konyvtar"}, // peer
	}
	_, err := matchSession(sessions, "abcd1234@")
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
		{ID: "sess-abcd1234", Peer: "konyvtar"},
		{ID: "sess-ab98ef76", Peer: "bespin"},
	}
	_, err := matchSession(sessions, "ab")
	if err == nil {
		t.Fatal("expected error for prefix matching multiple peer sessions")
	}
	msg := err.Error()
	// Both qualified forms must appear; the user uses the message to
	// pick the right one and retypes.
	if !strings.Contains(msg, "abcd1234@konyvtar") || !strings.Contains(msg, "ab98ef76@bespin") {
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
