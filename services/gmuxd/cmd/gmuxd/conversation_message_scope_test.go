package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// markSessionExited records an exit on a fixture session, standing in for a
// runner that is gone: the session row and its conversation ref survive, which
// is the retained-dead state `gmux agent output` must still answer for.
//
// Liveness itself lives in the registry, which this handler is not given —
// that absence IS the no-resume guarantee, so the fixture only has to prove
// the row says "exited" and still carries its conversation ref.
func markSessionExited(t *testing.T, f *conversationFixture, id string) {
	t.Helper()
	exited := centralstore.UnixMillis(2)
	if _, _, err := f.sessions.RegisterRunner(context.Background(), centralstore.RunnerRegistration{
		ID: centralstore.SessionID(id), Adapter: "pi", Alive: false, CreatedAt: 1, ObservedAt: 2,
		Facts: centralstore.RunnerFacts{ExitedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &exited}},
	}); err != nil {
		t.Fatalf("mark exited: %v", err)
	}
	row, found, err := f.sessions.Session(context.Background(), centralstore.SessionID(id))
	if err != nil || !found {
		t.Fatalf("reread %s: found=%v err=%v", id, found, err)
	}
	if row.ExitedAt == nil {
		t.Fatalf("%s has no exit recorded; the dead-session read would not be exercised", id)
	}
	if row.ConversationRef == "" {
		t.Fatalf("%s lost its conversation ref; the fixture no longer models a retained dead session", id)
	}
}

// TestConversationMessageScopeReturnsAssistantProse is the central claim of
// ADR 0027's semantic read: scope=message answers with the latest final
// assistant message — the prose only — and marks the response so a caller can
// tell a scope-aware daemon from one that ignored the parameter.
func TestConversationMessageScopeReturnsAssistantProse(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1",
		piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"run the tests"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"go test ./..."}}]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","content":[{"type":"toolCall","id":"t2","name":"bash","arguments":{"command":"go vet ./..."}},{"type":"text","text":"All green."}]}}`,
	)

	resp := f.do(http.MethodGet, "sess-1", "scope=message")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get(conversationScopeHeader); got != conversationScopeMessage {
		t.Errorf("%s: want %q, got %q", conversationScopeHeader, conversationScopeMessage, got)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "All green.\n" {
		t.Errorf("body: want %q, got %q", "All green.\n", body)
	}
}

// TestConversationMessageScopeSkipsToolOnlyTail: when the newest assistant
// message is tool calls only, the answer is the prose of the newest assistant
// message that has any — within the current turn. Tool lines are activity, not
// what the agent said, and a `[tool] bash {...}` line printed as the agent's
// reply is the exact contamination this selector exists to prevent.
func TestConversationMessageScopeSkipsToolOnlyTail(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1",
		piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix it"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"Done: the bug was an off-by-one."}]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"git status"}}]}}`,
	)

	resp := f.do(http.MethodGet, "sess-1", "scope=message")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "Done: the bug was an off-by-one.\n" {
		t.Errorf("body = %q", body)
	}
}

// TestConversationMessageScopeNoMessageYet enumerates the "nothing to report"
// shapes and requires each to be distinguishable — never an empty 200, which a
// caller would read as "the agent answered with silence".
func TestConversationMessageScopeNoMessageYet(t *testing.T) {
	f := newConversationFixture(t)

	// A brand-new pi session: header only.
	f.addPiSession(t, "sess-fresh", piSessionHeader)
	// A turn in flight: the user's prompt is recorded, the agent has produced
	// only tool calls so far. The PREVIOUS turn's answer must not be served as
	// this turn's, so this is no_message rather than stale prose.
	f.addPiSession(t, "sess-inflight",
		piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"first"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"an older answer"}]}}`,
		`{"type":"message","id":"u2","message":{"role":"user","content":[{"type":"text","text":"second"}]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"ls"}}]}}`,
	)
	// Only the user has spoken.
	f.addPiSession(t, "sess-useronly",
		piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hello?"}]}}`,
	)

	for _, id := range []string{"sess-fresh", "sess-inflight", "sess-useronly"} {
		resp := f.do(http.MethodGet, id, "scope=message")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", id, resp.StatusCode)
			continue
		}
		if code := errCode(t, resp); code != codeNoMessage {
			t.Errorf("%s: code = %q, want %q", id, code, codeNoMessage)
		}
	}

	// A deleted conversation file is a storage fact, not an absent message.
	gone := f.addPiSession(t, "sess-deleted", piSessionHeader,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	resp := f.do(http.MethodGet, "sess-deleted", "scope=message")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted: status = %d", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "no_conversation" {
		t.Errorf("deleted: code = %q, want no_conversation", code)
	}
}

// TestConversationMessageScopeUnsupportedAdapter: an adapter with no
// conversation model gets an explicit unsupported answer, distinct from the
// transient "no conversation yet" shape, because waiting will never help.
func TestConversationMessageScopeUnsupportedAdapter(t *testing.T) {
	f := newConversationFixture(t)
	f.addSession(t, "sess-shell", "shell", "")
	f.addSession(t, "sess-shell-ref", "shell", filepath.Join(f.dir, "whatever.jsonl"))

	for _, id := range []string{"sess-shell", "sess-shell-ref"} {
		resp := f.do(http.MethodGet, id, "scope=message")
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", id, resp.StatusCode)
			continue
		}
		if code := errCode(t, resp); code != codeUnsupportedAdapter {
			t.Errorf("%s: code = %q, want %q", id, code, codeUnsupportedAdapter)
		}
	}
}

// TestConversationScopeValidation: the default scope keeps the pre-existing
// transcript behavior byte for byte, an explicit transcript scope is accepted,
// and anything else is refused rather than served as a transcript.
func TestConversationScopeValidation(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1",
		piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`,
	)

	want := "## User\n\nhi\n\n## Assistant\n\nhello\n"
	for _, raw := range []string{"", "scope=transcript", "tail=2"} {
		resp := f.do(http.MethodGet, "sess-1", raw)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%q: status = %d", raw, resp.StatusCode)
		}
		if resp.Header.Get(conversationScopeHeader) != "" {
			t.Errorf("%q: transcript responses must not carry the message-scope marker", raw)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != want {
			t.Errorf("%q: body = %q, want the unchanged transcript %q", raw, body, want)
		}
	}

	// Presence is judged on the query key, not the value: an empty or repeated
	// scope is a caller whose intent did not survive, and resolving it to the
	// transcript default would hand a script the whole conversation where it
	// asked for one answer.
	for _, raw := range []string{
		"scope=messages", "scope=MESSAGE", "scope=turn",
		"scope=", "scope=message&scope=transcript", "scope=message&scope=message",
	} {
		resp := f.do(http.MethodGet, "sess-1", raw)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", raw, resp.StatusCode)
		}
	}

	// tail is meaningless for a single message and must not be ignored — its
	// mere presence is the error, including a valueless `tail=`.
	for _, raw := range []string{"scope=message&tail=1", "scope=message&tail=", "scope=message&tail=abc"} {
		resp := f.do(http.MethodGet, "sess-1", raw)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", raw, resp.StatusCode)
		}
	}
}

// TestConversationMessageScopeNeedsNoRunner is the no-resume guarantee: the
// read is served for a session with no live runner at all, from adapter-owned
// storage. The handler's only dependency is the store — it is given no
// registry, coordinator or runner endpoint, so it cannot resume or drive
// anything even by accident.
func TestConversationMessageScopeNeedsNoRunner(t *testing.T) {
	f := newConversationFixture(t)
	path := f.addPiSession(t, "sess-dead",
		piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"summarize"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"the final word"}]}}`,
	)
	markSessionExited(t, f, "sess-dead")

	resp := f.do(http.MethodGet, "sess-dead", "scope=message")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a dead session", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "the final word\n" {
		t.Errorf("body = %q", body)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("conversation file must be read, not moved: %v", err)
	}
}

// TestLatestAssistantProseSelector unit-pins the selector's edges, including
// the deliberate refusal to fall back to Text when an adapter reports no
// prose: that fallback is exactly how a tool line becomes "the answer".
func TestLatestAssistantProseSelector(t *testing.T) {
	tests := []struct {
		name  string
		msgs  []adapter.ConversationMessage
		want  string
		found bool
	}{
		{name: "empty transcript"},
		{name: "prose wins", msgs: []adapter.ConversationMessage{
			{Role: "user", Text: "q", Prose: "q"},
			{Role: "assistant", Text: "[tool] bash {}\n\nanswer", Prose: "answer"},
		}, want: "answer", found: true},
		{name: "tool-only tail falls back within the turn", msgs: []adapter.ConversationMessage{
			{Role: "user", Text: "q", Prose: "q"},
			{Role: "assistant", Text: "answer", Prose: "answer"},
			{Role: "assistant", Text: "[tool] bash {}", Prose: ""},
		}, want: "answer", found: true},
		{name: "never crosses the newest user message", msgs: []adapter.ConversationMessage{
			{Role: "assistant", Text: "old answer", Prose: "old answer"},
			{Role: "user", Text: "new question", Prose: "new question"},
			{Role: "assistant", Text: "[tool] bash {}", Prose: ""},
		}},
		{name: "no prose declared is not a fallback to Text", msgs: []adapter.ConversationMessage{
			{Role: "assistant", Text: "[tool] bash {}", Prose: ""},
		}},
		{name: "trailing newlines trimmed", msgs: []adapter.ConversationMessage{
			{Role: "assistant", Text: "answer\n\n", Prose: "answer\n\n"},
		}, want: "answer", found: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := latestAssistantProse(tt.msgs)
			if got != tt.want || found != tt.found {
				t.Errorf("= (%q, %v), want (%q, %v)", got, found, tt.want, tt.found)
			}
		})
	}
}
