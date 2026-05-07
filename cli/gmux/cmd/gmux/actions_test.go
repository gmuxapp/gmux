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

// TestBuildSendBody pins the wire-level contract of --send: by default
// the bytes written to the PTY end with the carriage return that
// submits the input, and --no-submit suppresses exactly that byte and
// nothing else. Both inline-text and stdin paths are covered because
// they construct the body differently and the carriage-return logic
// has to wrap both.
func TestBuildSendBody(t *testing.T) {
	tests := []struct {
		name     string
		text     *string
		stdin    string
		noSubmit bool
		want     string
	}{
		{
			name: "inline text submits with trailing \\r",
			text: stringPtr("hello"),
			want: "hello\r",
		},
		{
			name:     "inline text with --no-submit sends verbatim",
			text:     stringPtr("hello"),
			noSubmit: true,
			want:     "hello",
		},
		{
			name:  "stdin submits with trailing \\r",
			stdin: "prompt body\nwith newline\n",
			want:  "prompt body\nwith newline\n\r",
		},
		{
			name:     "stdin with --no-submit preserves trailing newline only",
			stdin:    "prompt body\nwith newline\n",
			noSubmit: true,
			want:     "prompt body\nwith newline\n",
		},
		{
			name: "empty inline text still submits an empty line",
			text: stringPtr(""),
			want: "\r",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := buildSendBody(tc.text, strings.NewReader(tc.stdin), tc.noSubmit)
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
