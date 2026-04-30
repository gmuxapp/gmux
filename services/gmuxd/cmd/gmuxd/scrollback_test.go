package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// brokerFixture wires up a real store + a temp dir keyed by session
// ID, then returns a closure that runs requests against the handler
// the same way the dispatcher does at runtime.
type brokerFixture struct {
	sessions *store.Store
	rootDir  string
	dirFor   func(string) string
}

func newBrokerFixture(t *testing.T) *brokerFixture {
	t.Helper()
	root := t.TempDir()
	return &brokerFixture{
		sessions: store.New(),
		rootDir:  root,
		dirFor:   func(id string) string { return filepath.Join(root, id) },
	}
}

func (f *brokerFixture) addSession(t *testing.T, id string) {
	t.Helper()
	f.sessions.Upsert(store.Session{ID: id, Kind: "shell", Alive: true})
}

func (f *brokerFixture) writeScrollback(t *testing.T, id, body string) {
	t.Helper()
	dir := f.dirFor(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, scrollback.ActiveName), []byte(body), 0o600); err != nil {
		t.Fatalf("write active: %v", err)
	}
}

func (f *brokerFixture) do(method, sessionID string) *http.Response {
	req := httptest.NewRequest(method, "/v1/sessions/"+sessionID+"/scrollback", nil)
	rec := httptest.NewRecorder()
	scrollbackBrokerHandler(rec, req, sessionID, f.sessions, f.dirFor)
	return rec.Result()
}

// TestBrokerStreamsScrollbackBytes is the central correctness claim:
// when a session has a scrollback file, GET returns it byte-for-byte.
// Without this, dead-session replay would diverge from what the
// runner actually emitted.
func TestBrokerStreamsScrollbackBytes(t *testing.T) {
	f := newBrokerFixture(t)
	f.addSession(t, "sess-1")

	want := "hello\x1b[31mred\x1b[0m\nworld\n"
	f.writeScrollback(t, "sess-1", want)

	resp := f.do(http.MethodGet, "sess-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type: want application/octet-stream, got %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control: want no-store, got %q", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != want {
		t.Errorf("body: want %q, got %q", want, body)
	}
}

// TestBrokerKnownSessionEmptyScrollbackReturns200 nails the design
// decision to NOT 404 when the session is known but has no
// scrollback yet. A frontend polling on attach would otherwise have
// to retry on 404 / treat 404 as transient, which is gnarly. With
// 200-empty, the polling logic is just "stream until eof, append".
func TestBrokerKnownSessionEmptyScrollbackReturns200(t *testing.T) {
	f := newBrokerFixture(t)
	f.addSession(t, "sess-fresh")
	// Note: no writeScrollback call. Session is in store, no files
	// on disk.

	resp := f.do(http.MethodGet, "sess-fresh")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	// Headers must still match the success case so a frontend can
	// rely on Content-Type/Cache-Control regardless of body length.
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type on empty body: want application/octet-stream, got %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control on empty body: want no-store, got %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("body: want empty, got %q", body)
	}
}

// TestBrokerUnknownSession404 distinguishes "session never existed"
// from "session known, no scrollback yet" (200 above). The frontend
// uses 404 to drop the session from its UI; conflating them would
// hide real sessions during the milliseconds before scrollback
// shows up on disk.
func TestBrokerUnknownSession404(t *testing.T) {
	f := newBrokerFixture(t)
	// No session added.

	resp := f.do(http.MethodGet, "sess-ghost")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", resp.StatusCode)
	}
}

// TestBrokerRejectsNonGet locks down readonly semantics. POST /
// PUT / DELETE on this endpoint should never succeed; the runner
// is the only writer.
func TestBrokerRejectsNonGet(t *testing.T) {
	f := newBrokerFixture(t)
	f.addSession(t, "sess-1")
	f.writeScrollback(t, "sess-1", "data")

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		resp := f.do(method, "sess-1")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: want 405, got %d", method, resp.StatusCode)
		}
	}
}

// TestBrokerConcatenatesPreviousAndActive verifies the rotation
// contract surfaces correctly through the broker: previous file
// bytes precede active file bytes. A regression here would replay
// rotated history out of order, putting the cursor in the wrong
// place and corrupting the visible scrollback.
func TestBrokerConcatenatesPreviousAndActive(t *testing.T) {
	f := newBrokerFixture(t)
	f.addSession(t, "sess-1")

	dir := f.dirFor("sess-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, scrollback.PreviousName), []byte("EARLIER\n"), 0o600); err != nil {
		t.Fatalf("write previous: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, scrollback.ActiveName), []byte("LATER\n"), 0o600); err != nil {
		t.Fatalf("write active: %v", err)
	}

	resp := f.do(http.MethodGet, "sess-1")
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "EARLIER\nLATER\n" {
		t.Errorf("ordering: want %q, got %q", "EARLIER\nLATER\n", body)
	}
}
