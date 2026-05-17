package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
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
// Optional ?tail=<N> query param trims the response to the last N
// newline-delimited lines (raw bytes; CRLF and ANSI preserved).
// Negative or non-numeric N is rejected with 400 so a typo doesn't
// silently fall through to "stream everything" and surprise the
// caller with a multi-MiB body. Absent param keeps current
// stream-everything behavior for the web UI.
//
// dirFor maps a session ID to its per-session directory. In
// production this is sessionmeta.Store.SessionDir; tests inject a
// closure pointing at a temp dir.
func scrollbackBrokerHandler(
	w http.ResponseWriter,
	r *http.Request,
	sessionID string,
	sessions *store.Store,
	dirFor func(string) string,
) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}
	if _, ok := sessions.Get(sessionID); !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}

	// tailN > 0 switches to line-tail mode (used by `gmux --tail`).
	// tailN == 0 means "no tail param given"; stream everything
	// (the contract the web UI consumes).
	tailN := 0
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "tail must be a positive integer")
			return
		}
		tailN = n
	}

	rc, err := scrollback.OpenReader(dirFor(sessionID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("scrollback: %s: %v", sessionID, err)
		writeError(w, http.StatusInternalServerError, "internal", "scrollback unavailable")
		return
	}

	// From here on the response status is 200. Set headers before any
	// io.Copy; the empty-body case (rc == nil) still sends them so the
	// frontend can distinguish a known-empty session from a 404.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")

	if rc == nil {
		return
	}
	defer rc.Close()

	if tailN > 0 {
		// Buffer-and-trim: scrollback is bounded at 2 * MaxBytes per
		// session, so reading it all into memory for a tail is fine
		// and lets us produce the response in a single Write.
		tail, err := scrollback.TailBytes(rc, tailN)
		if err != nil {
			log.Printf("scrollback tail: %s: %v", sessionID, err)
			// Headers were already sent (200 + Content-Type). The best
			// we can do is end the response; the client sees a short
			// read and surfaces it. Logging here is what makes the
			// failure debuggable.
			return
		}
		_, _ = w.Write(tail)
		return
	}

	// A mid-stream client disconnect (e.g. user closed the tab)
	// surfaces as a Copy error and is not actionable; nothing to log.
	_, _ = io.Copy(w, rc)
}
