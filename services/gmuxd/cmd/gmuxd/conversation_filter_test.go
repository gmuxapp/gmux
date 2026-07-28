package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// filterFixtureLines is a transcript with one of each type: a user message, an
// assistant message that only calls tools, and an assistant message that mixes
// a tool call with prose.
func filterFixtureLines() []string {
	return []string{
		piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"run the tests"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"go test ./..."}}]}}`,
		`{"type":"message","id":"tr","message":{"role":"toolResult","content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","content":[{"type":"thinking","text":"hmm"},{"type":"toolCall","id":"t2","name":"bash","arguments":{"command":"go vet ./..."}},{"type":"text","text":"All green."}]}}`,
	}
}

// TestConversationTypeFilter is the read behind `gmux agent logs`' filters: the
// default set is user + prose-bearing agent messages (tool-only and toolResult
// messages excluded), explicit types REPLACE that set, and they compose.
func TestConversationTypeFilter(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1", filterFixtureLines()...)

	for _, tt := range []struct {
		query      string
		wantHave   []string
		wantAbsent []string
	}{
		{"", []string{"run the tests", "All green."}, []string{"go test ./..."}},
		{"types=user", []string{"run the tests"}, []string{"All green.", "go test ./..."}},
		{"types=agent", []string{"All green."}, []string{"run the tests"}},
		{"types=tool", []string{"go test ./..."}, []string{"run the tests"}},
		{"types=user,tool", []string{"run the tests", "go test ./..."}, []string{"All green."}},
		{"types=user,agent,tool", []string{"run the tests", "All green.", "go test ./..."}, nil},
	} {
		t.Run("?"+tt.query, func(t *testing.T) {
			resp := f.do(http.MethodGet, "sess-1", tt.query)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			for _, want := range tt.wantHave {
				if !strings.Contains(string(body), want) {
					t.Errorf("body missing %q:\n%s", want, body)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(string(body), absent) {
					t.Errorf("body must not contain %q:\n%s", absent, body)
				}
			}
			// A toolResult echo is never a message type: the renderer drops it
			// upstream, and no filter may resurrect it.
			if strings.Contains(string(body), `"ok"`) {
				t.Errorf("toolResult content leaked into the transcript:\n%s", body)
			}
		})
	}
}

// TestConversationTailCountsPostFilter: tail counts the messages the caller
// asked to see. Counting pre-filter would make `--tool -n 1` mean "the last
// message, if it happens to be a tool call".
func TestConversationTailCountsPostFilter(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1", filterFixtureLines()...)
	resp := f.do(http.MethodGet, "sess-1", "types=tool&tail=1")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "go test ./...") {
		t.Fatalf("status=%d body=%s, want the single tool message", resp.StatusCode, body)
	}
	// And a filter that matches nothing is an explicit code, never an empty
	// success: printing nothing under a 200 would say the agent has done none of
	// this.
	f.addPiSession(t, "sess-2", piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`)
	resp = f.do(http.MethodGet, "sess-2", "types=tool")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a filter that matches nothing", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), codeNoMessage) {
		t.Errorf("body = %s, want %s", body, codeNoMessage)
	}
}

// TestConversationMissingFileIsEmptyNotGone: pi reports its conversation path
// before writing the first line, so a brand-new session (or one whose first
// turn is still running) has a ref pointing at nothing. Reporting that as
// "conversation is gone" told the caller their transcript had been destroyed —
// `gmux agent status` on a session that has just been launched hit exactly this.
func TestConversationMissingFileIsEmptyNotGone(t *testing.T) {
	f := newConversationFixture(t)
	f.addSession(t, "sess-new", "pi", f.dir+"/never-written.jsonl")
	resp := f.do(http.MethodGet, "sess-new", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "no messages yet") || strings.Contains(string(body), "gone") {
		t.Errorf("body = %s, want it to say the conversation has no messages yet", body)
	}
}

// TestConversationJSONFormat pins the machine contract of `logs --json` and the
// shape `agent status` reads: role, type, text and the prose subset.
func TestConversationJSONFormat(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1", filterFixtureLines()...)
	resp := f.do(http.MethodGet, "sess-1", "types=user,agent,tool&format=json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Messages []struct {
				Role  string `json:"role"`
				Type  string `json:"type"`
				Text  string `json:"text"`
				Prose string `json:"prose"`
			} `json:"messages"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msgs := env.Data.Messages
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Type != messageTypeUser {
		t.Errorf("first message = %+v", msgs[0])
	}
	if msgs[1].Type != messageTypeTool || msgs[1].Prose != "" {
		t.Errorf("a tool-only message must have no prose: %+v", msgs[1])
	}
	// A mixed message stays ONE message: text carries the tool rendering too,
	// prose carries only what the agent said. Splitting it would invent messages
	// the adapter never asserted.
	if msgs[2].Type != messageTypeAgent || msgs[2].Prose != "All green." || !strings.Contains(msgs[2].Text, "[tool] bash") {
		t.Errorf("mixed message = %+v", msgs[2])
	}
	// Thinking is not rendered by this adapter, so no type can surface it.
	for _, m := range msgs {
		if strings.Contains(m.Text, "hmm") {
			t.Errorf("thinking leaked into %+v", m)
		}
	}
}

// TestConversationJSONAlwaysCarriesEveryField: the documented shape is
// {role, type, text, prose}, and `logs --json` re-emits the daemon's objects
// verbatim while `status --json` re-marshals through the CLI's own struct. If
// prose were omitted for a tool-only message, `.prose` would be null in one
// contract and "" in the other for the same message.
func TestConversationJSONAlwaysCarriesEveryField(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1", filterFixtureLines()...)
	resp := f.do(http.MethodGet, "sess-1", "types=tool&format=json")
	var env struct {
		Data struct {
			Messages []map[string]any `json:"messages"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Messages) != 1 {
		t.Fatalf("got %d messages, want the one tool message", len(env.Data.Messages))
	}
	for _, field := range []string{"role", "type", "text", "prose"} {
		if _, present := env.Data.Messages[0][field]; !present {
			t.Errorf("tool-only message omits %q: %v", field, env.Data.Messages[0])
		}
	}
}

// TestConversationMessageScopeMissingFileIsEmptyNotGone: both scopes tell the
// same ENOENT truth. The transcript scope shadows this one for `agent status`
// today by read ordering alone; a direct caller of scope=message must not be
// told a brand-new session's transcript was destroyed.
func TestConversationMessageScopeMissingFileIsEmptyNotGone(t *testing.T) {
	f := newConversationFixture(t)
	f.addSession(t, "sess-new", "pi", f.dir+"/never-written.jsonl")
	resp := f.do(http.MethodGet, "sess-new", "scope=message")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "no messages yet") || strings.Contains(string(body), "gone") {
		t.Errorf("body = %s, want it to say the conversation has no messages yet", body)
	}
}

// TestConversationFilterValidation: a mistyped filter or format is refused, not
// silently resolved to a default. A narrowed transcript served as a success
// reads as "the agent did none of that".
func TestConversationFilterValidation(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1", filterFixtureLines()...)
	for _, tt := range []struct{ query, want string }{
		{"types=thinking", "unknown message type"},
		{"types=", "must not be empty"},
		{"types=user&types=tool", "at most once"},
		{"types=user,", "empty entry"},
		{"format=yaml", "unknown format"},
		{"format=", "must not be empty"},
	} {
		t.Run("?"+tt.query, func(t *testing.T) {
			resp := f.do(http.MethodGet, "sess-1", tt.query)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), tt.want) {
				t.Errorf("body = %s, want it to mention %q", body, tt.want)
			}
		})
	}
}
