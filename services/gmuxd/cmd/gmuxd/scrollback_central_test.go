package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// brokerFixture wires up a real store + a temp dir keyed by session
// ID, then returns a closure that runs requests against the handler
// the same way the dispatcher does at runtime.
type brokerFixture struct {
	sessions map[string]wire.Session
	rootDir  string
	dirFor   func(string) string
}

func newBrokerFixture(t *testing.T) *brokerFixture {
	t.Helper()
	root := t.TempDir()
	return &brokerFixture{
		sessions: make(map[string]wire.Session),
		rootDir:  root,
		dirFor:   func(id string) string { return filepath.Join(root, id) },
	}
}

func (f *brokerFixture) addSession(t *testing.T, id string) {
	t.Helper()
	f.sessions[id] = wire.Session{ID: id, Adapter: "shell", Alive: true}
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
	return f.doQuery(method, sessionID, "")
}

// doQuery is the same as do but lets a test attach a raw query
// string (e.g. "tail=2") to exercise the optional knobs of the
// endpoint.
func (f *brokerFixture) doQuery(method, sessionID, rawQuery string) *http.Response {
	url := "/v1/sessions/" + sessionID + "/scrollback"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(method, url, nil)
	rec := httptest.NewRecorder()
	sess, ok := f.sessions[sessionID]
	scrollbackBrokerHandlerCentral(rec, req, sessionID, sess, ok, f.dirFor)
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

// TestBrokerTailParamTrimsToLastNLines is the contract `gmux --tail`
// relies on: passing tail=N must drop everything before the trailing
// N lines, byte-for-byte. The previous file + active file
// concatenation must be tailed as a single logical stream so the
// rotation boundary doesn't leak into the response.
func TestBrokerTailParamTrimsToLastNLines(t *testing.T) {
	f := newBrokerFixture(t)
	f.addSession(t, "sess-1")

	dir := f.dirFor("sess-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// CRLF mirrors what the PTY actually emits; bare LF without CR
	// would leave the cursor at column N when the emulator processes
	// the next line, producing staircased output. Real on-disk
	// scrollback is always CRLF.
	//
	// 3 lines in previous + 3 in active; tail=4 must cross the
	// rotation boundary correctly.
	if err := os.WriteFile(filepath.Join(dir, scrollback.PreviousName), []byte("p1\r\np2\r\np3\r\n"), 0o600); err != nil {
		t.Fatalf("write previous: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, scrollback.ActiveName), []byte("a1\r\na2\r\na3\r\n"), 0o600); err != nil {
		t.Fatalf("write active: %v", err)
	}

	resp := f.doQuery(http.MethodGet, "sess-1", "tail=4")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type: want text/plain, got %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	want := "p3\na1\na2\na3\n"
	if string(body) != want {
		t.Errorf("body: want %q, got %q", want, body)
	}
}

// TestBrokerTailParamStripsANSI is the end-to-end version of the
// rendering claim: bytes the child wrote with ANSI styling come out
// of the broker as plain text. If the broker ever stopped routing
// through the emulator and reverted to raw byte tailing, `gmux --tail`
// output would suddenly include escape sequences and break log-style
// consumption.
func TestBrokerTailParamStripsANSI(t *testing.T) {
	f := newBrokerFixture(t)
	f.addSession(t, "sess-1")
	f.writeScrollback(t, "sess-1", "\x1b[31mred line\x1b[0m\r\n\x1b[32mgreen line\x1b[0m\r\n")

	resp := f.doQuery(http.MethodGet, "sess-1", "tail=2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if bytes.ContainsRune(body, 0x1b) {
		t.Errorf("body still contains an ANSI escape byte: %q", body)
	}
	want := "red line\ngreen line\n"
	if string(body) != want {
		t.Errorf("body: want %q, got %q", want, body)
	}
}

// TestBrokerTailParamCollapsesCursorOverwrites is the case that
// justifies replaying through a real emulator instead of byte-tailing.
// A child that prints "loading...\r" and then "done      \r\n"
// overwrites the same row; the user expects --tail to surface the
// final visible content ("done"), not the byte history of the
// overwrite. Byte-tailing would return both fragments concatenated.
func TestBrokerTailParamCollapsesCursorOverwrites(t *testing.T) {
	f := newBrokerFixture(t)
	f.addSession(t, "sess-1")
	f.writeScrollback(t, "sess-1", "loading...\rdone      \r\n")

	resp := f.doQuery(http.MethodGet, "sess-1", "tail=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, "done") {
		t.Errorf("want %q in output, got %q", "done", text)
	}
	if strings.Contains(text, "loading") {
		t.Errorf("overwritten %q should not appear, got %q", "loading", text)
	}
}

// TestBrokerTailParamEmptySession returns 200 with empty body when a
// session is known but has no scrollback. tail=N must not change
// that: a fresh session is still a known session, just one with
// nothing to show.
func TestBrokerTailParamEmptySession(t *testing.T) {
	f := newBrokerFixture(t)
	f.addSession(t, "sess-empty")

	resp := f.doQuery(http.MethodGet, "sess-empty", "tail=10")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("body: want empty, got %q", body)
	}
}

// TestBrokerTailParamRejectsBadValue locks down the input contract.
// A non-numeric or non-positive tail must 400, not fall through to
// "stream everything" — a typo like `?tail=abc` returning a multi-MiB
// body would be a footgun, and `?tail=0` is meaningless.
func TestBrokerTailParamRejectsBadValue(t *testing.T) {
	f := newBrokerFixture(t)
	f.addSession(t, "sess-1")
	f.writeScrollback(t, "sess-1", "data\n")

	for _, raw := range []string{"tail=abc", "tail=0", "tail=-3", "tail="} {
		resp := f.doQuery(http.MethodGet, "sess-1", raw)
		// tail= with empty value is the one ambiguous case. The
		// handler treats it as "param not given" (current behavior:
		// stream everything) because url.Values returns the same
		// empty string whether the param was absent or present-empty.
		// Document that explicitly here so a future change doesn't
		// silently make it stricter without thought.
		if raw == "tail=" {
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s: want 200 (treated as absent), got %d", raw, resp.StatusCode)
			}
			continue
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", raw, resp.StatusCode)
		}
	}
}

// TestBrokerTailParamIOErrorReturns500 guards against the silent-200
// bug where a TailBytes read failure would fall through with no Write
// having occurred, and the HTTP server would flush a default 200 OK
// with empty body — leaving the client to interpret a read failure as
// "no scrollback yet." The handler must commit a 5xx explicitly so the
// failure surfaces.
//
// To exercise specifically the TailBytes error path (not the
// OpenReader error path), seed the scrollback location with a
// directory: os.Open on a directory succeeds, but a subsequent Read
// on it fails with EISDIR. That's what TailBytes -> io.ReadAll sees.
func TestBrokerTailParamIOErrorReturns500(t *testing.T) {
	f := newBrokerFixture(t)
	f.addSession(t, "sess-1")

	dir := f.dirFor("sess-1")
	// Put a directory where the active scrollback file should be.
	// OpenReader will Open it (success), TailBytes will ReadAll it
	// (fails with EISDIR).
	if err := os.MkdirAll(filepath.Join(dir, scrollback.ActiveName), 0o700); err != nil {
		t.Fatalf("mkdir trap: %v", err)
	}

	resp := f.doQuery(http.MethodGet, "sess-1", "tail=5")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status: want 500, got %d (a silent 200 here would let the CLI report success on an IO error)", resp.StatusCode)
	}
}
