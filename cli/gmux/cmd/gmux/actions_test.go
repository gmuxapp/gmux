package main

import (
	"encoding/json"
	"io"
	"net/http"
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
		{"DCS payload removed", "before\x1bPq?sixel-data\x1b\\after", "beforeafter"},
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

func TestListJSONCommandGolden(t *testing.T) {
	exit := 0
	tests := []struct {
		name     string
		sessions []cliSession
		all      bool
		want     string
	}{
		{
			name: "empty is an array",
			want: "[]\n",
		},
		{
			name:     "minimal local",
			sessions: []cliSession{{ID: "1va8lvdv", Adapter: "shell", Alive: true}},
			want:     "[\n  {\n    \"ref\": \"1va8lvdv\",\n    \"id\": \"1va8lvdv\",\n    \"adapter\": \"shell\",\n    \"alive\": true\n  }\n]\n",
		},
		{
			name: "full peer",
			all:  true,
			sessions: []cliSession{{
				ID: "1va8lvdv", Peer: "work-laptop", Cwd: "/home/mg/dev/gmux",
				Adapter: "pi", Alive: false, Pid: 4242, Title: "fix auth bug",
				Slug: "fix-auth-bug", RunnerVersion: "2.0.0", ParentSessionID: "1u0xpj5g",
				SocketPath: "/run/gmux/1va8lvdv.sock", Command: []string{"pi", "--model", "sonnet"},
				StartedAt: "2026-07-27T10:00:00.123Z", ExitedAt: "2026-07-27T10:05:00Z", ExitCode: &exit,
			}},
			want: "[\n  {\n    \"ref\": \"1va8lvdv@work-laptop\",\n    \"id\": \"1va8lvdv\",\n    \"peer\": \"work-laptop\",\n    \"cwd\": \"/home/mg/dev/gmux\",\n    \"adapter\": \"pi\",\n    \"alive\": false,\n    \"pid\": 4242,\n    \"title\": \"fix auth bug\",\n    \"slug\": \"fix-auth-bug\",\n    \"runner_version\": \"2.0.0\",\n    \"parent_session_id\": \"1u0xpj5g\",\n    \"socket_path\": \"/run/gmux/1va8lvdv.sock\",\n    \"command\": [\n      \"pi\",\n      \"--model\",\n      \"sonnet\"\n    ],\n    \"started_at\": \"2026-07-27T10:00:00.123Z\",\n    \"exited_at\": \"2026-07-27T10:05:00Z\",\n    \"exit_code\": 0\n  }\n]\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			startStubDaemon(t, tc.sessions)
			var stdout string
			stderr := captureStderr(t, func() {
				stdout = captureStdout(t, func() {
					if code := cmdList(tc.all, true); code != 0 {
						t.Errorf("cmdList exit = %d, want 0", code)
					}
				})
			})
			if stdout != tc.want {
				t.Errorf("stdout mismatch\ngot:\n%s\nwant:\n%s", stdout, tc.want)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
		})
	}
}

func TestListTableUsesContractOrdering(t *testing.T) {
	startStubDaemon(t, []cliSession{
		{ID: "dead-new", Adapter: "shell", StartedAt: "2026-07-31T12:00:00Z"},
		{ID: "live-old", Adapter: "pi", Alive: true, StartedAt: "2026-07-30T12:00:00Z"},
		{ID: "live-new", Adapter: "pi", Alive: true, StartedAt: "2026-07-31T12:00:00Z"},
	})
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			if code := cmdList(false, false); code != 0 {
				t.Errorf("cmdList exit = %d, want 0", code)
			}
		})
	})
	want := "ID        STATUS  ADAPTER  TITLE\n" +
		"live-new  alive   pi       \n" +
		"live-old  alive   pi       \n" +
		"dead-new  dead    shell    \n"
	if stdout != want {
		t.Errorf("stdout mismatch\ngot:\n%q\nwant:\n%q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestListJSONOrderingResultsNullNormalizationAndRefs(t *testing.T) {
	startStubDaemon(t, nil).onSessions(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"data":[
			{"id":"dead-zero","adapter":"shell","alive":false,"started_at":"2026-07-30T10:00:00Z","exited_at":"2026-07-30T10:01:00Z","exit_code":0},
			{"id":"live-b","peer":"tower","adapter":"pi","alive":true,"started_at":"2026-07-31T10:00:00Z","pid":null,"exited_at":null,"exit_code":null,"binary_hash":"private"},
			{"id":"dead-unknown","adapter":"shell","alive":false,"started_at":"2026-07-31T11:00:00Z"},
			{"id":"live-a","adapter":"pi","alive":true,"started_at":"2026-07-31T12:00:00+02:00"}
		]}`)
	})
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			if code := cmdList(true, true); code != 0 {
				t.Fatalf("cmdList exit = %d", code)
			}
		})
	})
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("stdout is not one JSON array: %v\n%s", err, stdout)
	}
	wantRefs := []string{"live-a", "live-b@tower", "dead-unknown", "dead-zero"}
	if len(rows) != len(wantRefs) {
		t.Fatalf("rows = %d, want %d", len(rows), len(wantRefs))
	}
	for i, wantRef := range wantRefs {
		var ref string
		if err := json.Unmarshal(rows[i]["ref"], &ref); err != nil || ref != wantRef {
			t.Errorf("row %d ref = %q (%v), want %q", i, ref, err, wantRef)
		}
		for _, key := range []string{"pid", "exited_at", "exit_code"} {
			if string(rows[i][key]) == "null" {
				t.Errorf("row %d emits %s:null; optional fields must be omitted", i, key)
			}
		}
		if _, ok := rows[i]["binary_hash"]; ok {
			t.Errorf("row %d exposes binary_hash", i)
		}
	}
	if _, ok := rows[3]["exit_code"]; !ok {
		t.Error("known zero exit_code was omitted")
	}
	if _, ok := rows[2]["exit_code"]; ok {
		t.Error("unknown exit_code was emitted")
	}

	// Every emitted ref must round-trip through the same resolver used by
	// tail/send/wait/kill/agent verbs.
	sessions, err := fetchSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range wantRefs {
		if _, err := matchSession(sessions, ref); err != nil {
			t.Errorf("emitted ref %q is not reusable: %v", ref, err)
		}
	}
}

func TestFetchSessionsRejectsAmbiguousPeerName(t *testing.T) {
	startStubDaemon(t, nil).onSessions(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"data":[{"id":"abc12345","peer":"bad@peer","adapter":"pi","alive":true}]}`)
	})
	if _, err := fetchSessions(); err == nil || !strings.Contains(err.Error(), "invalid peer name") {
		t.Fatalf("fetchSessions error = %v, want invalid peer name", err)
	}
}

// This test pins existing fields and their types while deliberately allowing
// additive keys in future 2.x releases.
func TestListJSONSchemaIsStable(t *testing.T) {
	exit := 0
	full := cliSession{
		ID: "1va8lvdv", Peer: "laptop", Cwd: "/work", Adapter: "pi", Alive: true,
		Pid: 42, Title: "title", Slug: "slug", RunnerVersion: "2.0.0",
		ParentSessionID: "parent", SocketPath: "/run/socket", Command: []string{"pi"},
		StartedAt: "2026-07-27T10:00:00Z", ExitedAt: "2026-07-27T10:05:00Z", ExitCode: &exit,
	}
	got := jsonKeys(t, full)
	wantTypes := map[string]byte{
		"ref": '"', "id": '"', "peer": '"', "cwd": '"', "adapter": '"', "alive": 't',
		"pid": '4', "title": '"', "slug": '"', "runner_version": '"', "parent_session_id": '"',
		"socket_path": '"', "command": '[', "started_at": '"', "exited_at": '"', "exit_code": '0',
	}
	for key, firstByte := range wantTypes {
		value, ok := got[key]
		if !ok {
			t.Errorf("populated session omits documented key %q; keys: %v", key, sortedKeys(got))
		} else if len(value) == 0 || value[0] != firstByte {
			t.Errorf("key %q has value %s, want JSON type beginning %q", key, value, firstByte)
		}
	}

	bare := jsonKeys(t, cliSession{ID: "1va8lvdv"})
	for _, key := range []string{"ref", "id", "adapter", "alive"} {
		if _, ok := bare[key]; !ok {
			t.Errorf("minimal session omits required key %q", key)
		}
	}
	for _, key := range []string{"peer", "cwd", "pid", "title", "slug", "runner_version", "parent_session_id", "socket_path", "command", "started_at", "exited_at", "exit_code"} {
		if _, ok := bare[key]; ok {
			t.Errorf("optional key %q appears on a minimal session", key)
		}
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
