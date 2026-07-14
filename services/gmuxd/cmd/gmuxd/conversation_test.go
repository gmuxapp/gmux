package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// conversationFixture wires a real store plus temp conversation files
// and runs requests against conversationHandler the way the dispatcher
// does at runtime. The pi adapter is exercised for real (its JSONL
// parser is the production render path), so these tests double as the
// endpoint-level contract for pi transcripts.
type conversationFixture struct {
	sessions *store.Store
	dir      string
}

func newConversationFixture(t *testing.T) *conversationFixture {
	t.Helper()
	return &conversationFixture{sessions: store.New(), dir: t.TempDir()}
}

// addPiSession registers a pi session whose ConversationRef points at
// a JSONL file with the given lines. Returns the file path.
func (f *conversationFixture) addPiSession(t *testing.T, id string, lines ...string) string {
	t.Helper()
	path := filepath.Join(f.dir, id+".jsonl")
	var content string
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write conversation: %v", err)
	}
	f.sessions.Upsert(store.Session{ID: id, Adapter: "pi", Alive: true, ConversationRef: path})
	return path
}

func (f *conversationFixture) do(method, sessionID, rawQuery string) *http.Response {
	url := "/v1/sessions/" + sessionID + "/conversation"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(method, url, nil)
	rec := httptest.NewRecorder()
	conversationHandler(rec, req, sessionID, f.sessions)
	return rec.Result()
}

// errCode extracts the error envelope code the CLI keys its
// scrollback fallback on.
func errCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return env.Error.Code
}

// piSessionHeader is a minimal valid pi JSONL header line.
const piSessionHeader = `{"type":"session","version":3,"id":"abc-123","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`

// TestConversationRendersMarkdownTranscript is the endpoint's central
// claim: a pi session's JSONL comes back as a role-headed markdown
// transcript with text/markdown content type — the body `gmux tail`
// prints verbatim.
func TestConversationRendersMarkdownTranscript(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1",
		piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"run the tests"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"go test ./..."}},{"type":"text","text":"All green."}]}}`,
	)

	resp := f.do(http.MethodGet, "sess-1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type: want text/markdown, got %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control: want no-store, got %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	want := "## User\n\nrun the tests\n\n## Assistant\n\n[tool] bash {\"command\":\"go test ./...\"}\n\nAll green.\n"
	if string(body) != want {
		t.Errorf("body:\nwant %q\ngot  %q", want, body)
	}
}

// TestConversationTailParamTrimsToLastNMessages: ?tail=N is the
// message-unit analog of scrollback's last-N-lines — `gmux tail -n 1`
// must return only the newest message, not the newest line.
func TestConversationTailParamTrimsToLastNMessages(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1",
		piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"first"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"second"}]}}`,
	)

	resp := f.do(http.MethodGet, "sess-1", "tail=1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	want := "## Assistant\n\nsecond\n"
	if string(body) != want {
		t.Errorf("body: want %q, got %q", want, body)
	}
}

// TestConversationTailParamRejectsBadValue mirrors the scrollback
// broker's input contract: a typo must 400, not silently return the
// full transcript.
func TestConversationTailParamRejectsBadValue(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1", piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`)

	for _, raw := range []string{"tail=abc", "tail=0", "tail=-3"} {
		resp := f.do(http.MethodGet, "sess-1", raw)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", raw, resp.StatusCode)
		}
	}
}

// TestConversationUnknownSession404 keeps "session never existed"
// (code not_found, CLI reports an error) distinct from "session has no
// conversation" (code no_conversation, CLI falls back to scrollback).
func TestConversationUnknownSession404(t *testing.T) {
	f := newConversationFixture(t)
	resp := f.do(http.MethodGet, "sess-ghost", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "not_found" {
		t.Errorf("code: want not_found, got %q", code)
	}
}

// TestConversationFallbackSignals enumerates every "no renderable
// conversation" shape and pins them all to 404/no_conversation — the
// single signal the CLI keys its scrollback fallback on. If any of
// these leaked a different code, `gmux tail` would either error out or
// misreport the session as missing instead of showing PTY output.
func TestConversationFallbackSignals(t *testing.T) {
	f := newConversationFixture(t)

	// A shell session: no conversation file at all.
	f.sessions.Upsert(store.Session{ID: "sess-shell", Adapter: "shell", Alive: true})

	// A session whose adapter has a conversation file on record but does
	// not implement ConversationRenderer (shell never will; stands in
	// for any future filer-without-renderer adapter).
	f.sessions.Upsert(store.Session{ID: "sess-norender", Adapter: "shell", Alive: true,
		ConversationRef: filepath.Join(f.dir, "whatever.jsonl")})

	// A pi session whose conversation file was deleted.
	gone := f.addPiSession(t, "sess-deleted", piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	// A fresh pi session: header line only, no messages yet.
	f.addPiSession(t, "sess-fresh", piSessionHeader)

	for _, id := range []string{"sess-shell", "sess-norender", "sess-deleted", "sess-fresh"} {
		resp := f.do(http.MethodGet, id, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status: want 404, got %d", id, resp.StatusCode)
			continue
		}
		if code := errCode(t, resp); code != "no_conversation" {
			t.Errorf("%s: code: want no_conversation, got %q", id, code)
		}
	}
}

// TestConversationRejectsNonGet locks down readonly semantics, same as
// the scrollback broker.
func TestConversationRejectsNonGet(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1", piSessionHeader)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		resp := f.do(method, "sess-1", "")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: want 405, got %d", method, resp.StatusCode)
		}
	}
}
