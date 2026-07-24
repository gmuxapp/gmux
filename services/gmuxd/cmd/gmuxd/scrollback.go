package main

import (
	"io"
	"log"
	"net/http"

	"github.com/gmuxapp/gmux/packages/scrollback"
)

// scrollbackBrokerHandler streams a session's persisted PTY scrollback
// (previous file followed by active file, chronological order) as raw
// bytes. It's the readonly counterpart to the runner's tee in
// ptyserver: dead sessions whose runner socket is gone can still
// serve their terminal history.
//
// Status code semantics:
//
//   - 405 if the method isn't GET. Scrollback is read-only; writes
//     happen exclusively through the runner.
//   - 404 if the session ID isn't in the store. The session existing
//     in the store is the authorization gate: peer sessions get
//     forwarded to the owning gmuxd before reaching this handler.
//   - 200 with an empty body when the session is known but no
//     scrollback files exist. This is the "fresh session, runner
//     hasn't produced output yet" case (or a session whose runner
//     exited without writing anything). Distinguishing it from 404
//     means the frontend never has to retry on a known session.
//   - 500 on any other IO error reading the scrollback dir; the
//     log line carries the underlying cause.
//
// Optional ?tail=<N> query param switches the response to plain text:
// the last N lines rendered through a fresh terminal emulator (ANSI
// stripped, cursor overwrites collapsed to the final screen, blank
// trailing rows trimmed). This is the format `gmux --tail` consumes.
// Negative or non-numeric N is rejected with 400 so a typo doesn't
// silently fall through to "stream everything" and surprise the caller
// with a multi-MiB body. Absent param keeps current raw-stream
// behavior for the web UI (which renders in xterm.js client-side).
//
// dirFor maps a session ID to its per-session directory. In
// production this is sessionmeta.Store.SessionDir; tests inject a
// closure pointing at a temp dir.
// renderTail replays the on-disk scrollback through a fresh terminal
// emulator and writes the last n lines as plain text. The emulator
// gives `gmux --tail` the same shape of output the runner's now-
// removed /scrollback/tail used to: ANSI stripped, cursor overwrites
// collapsed to the final visible screen, blank trailing rows trimmed.
// Doing it here (against the on-disk tee) means dead sessions get the
// same treatment as live ones; the runner's live cell grid is no
// longer the only place that can produce a tail.
//
// Dimensions come from the session's last-known terminal size; on
// sessions that never emitted a resize the renderer falls back to
// 80x24, the same default the runner uses at launch. A small
// width mismatch can only affect wrap points, which doesn't make a
// log-style tail meaningfully wrong.
func renderTail(w http.ResponseWriter, rc io.ReadCloser, sess compatSession, sessionID string, n int) {
	if rc == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		return
	}
	defer rc.Close()

	lines, err := scrollback.RenderTail(rc, int(sess.TerminalCols), int(sess.TerminalRows), n)
	if err != nil {
		log.Printf("scrollback tail: %s: %v", sessionID, err)
		writeError(w, http.StatusInternalServerError, "internal", "scrollback render failed")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	for _, line := range lines {
		_, _ = w.Write([]byte(line))
		_, _ = w.Write([]byte{'\n'})
	}
}
