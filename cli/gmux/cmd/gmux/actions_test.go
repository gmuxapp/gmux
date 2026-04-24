package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadPersistedTail covers the on-disk fallback `gmux --tail` uses
// when the session's socket has gone away. ptyserver writes the full
// scrollback as plain text to <socketPath>.tail on exit; the CLI picks
// up the last N lines from that file so users can still peek at a
// finished make-build or pytest run without re-executing it.
func TestReadPersistedTail(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess-deadbeef.sock")
	tailPath := sockPath + ".tail"

	// 12 lines of known content, one per row.
	var content bytes.Buffer
	for i := 1; i <= 12; i++ {
		content.WriteString("line-" + twoDigit(i) + "\n")
	}
	if err := os.WriteFile(tailPath, content.Bytes(), 0o600); err != nil {
		t.Fatalf("write tail file: %v", err)
	}

	got, err := readPersistedTail(sockPath, 3)
	if err != nil {
		t.Fatalf("readPersistedTail: %v", err)
	}
	want := "line-10\nline-11\nline-12\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestReadPersistedTail_RequestMoreThanAvailable returns everything
// when the caller asks for more lines than the file has. Matches how
// the live /scrollback/tail endpoint behaves, so a caller script gets
// consistent output regardless of whether the session is still alive.
func TestReadPersistedTail_RequestMoreThanAvailable(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess-cafef00d.sock")
	tailPath := sockPath + ".tail"

	if err := os.WriteFile(tailPath, []byte("only\ntwo\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := readPersistedTail(sockPath, 100)
	if err != nil {
		t.Fatalf("readPersistedTail: %v", err)
	}
	if string(got) != "only\ntwo\n" {
		t.Errorf("got %q, want %q", got, "only\ntwo\n")
	}
}

// TestReadPersistedTail_MissingFile returns a distinguishable error
// (os.IsNotExist true) so the caller can tell "nothing persisted"
// apart from "disk corruption".
func TestReadPersistedTail_MissingFile(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess-nope.sock")

	_, err := readPersistedTail(sockPath, 10)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("err = %v, want an os.IsNotExist error", err)
	}
}

// TestLastNLines covers the byte-walk tail extractor. The one case
// most likely to be wrong in a hand-rolled implementation is input
// without a trailing newline: the "last line" still exists and must
// be counted, otherwise a truncated write silently drops content from
// the tail.
func TestLastNLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 5, ""},
		{"n=0", "a\nb\n", 0, ""},
		{"fewer lines than requested", "only\ntwo\n", 10, "only\ntwo\n"},
		{"exact tail with trailing newline", "a\nb\nc\n", 2, "b\nc\n"},
		{"tail without trailing newline", "a\nb\nc", 2, "b\nc"},
		{"single unterminated line", "solo", 1, "solo"},
		{"single unterminated line, ask for more", "solo", 5, "solo"},
		{"two lines no trailing, n=1", "a\nb", 1, "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(lastNLines([]byte(tc.in), tc.n))
			if got != tc.want {
				t.Errorf("lastNLines(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

func twoDigit(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

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
