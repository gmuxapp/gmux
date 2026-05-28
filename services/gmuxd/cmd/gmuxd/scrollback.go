package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// scrollbackBrokerHandler streams a session's persisted PTY scrollback
// (previous file followed by active file, chronological order) as raw
// bytes, or as extracted scrollback when ?extracted=1 is set.
//
// Query parameters:
//
//   - extracted=1: run the bytes through scrollback.ExtractBytes before
//     sending. The server does the heavy BSU/ESU block processing so the
//     client only downloads the compact, human-readable result (typically
//     ~1–2% of the raw file size for long pi sessions). The client must
//     write the result directly into the terminal emulator — no further
//     processing is needed on receipt.
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

	// ?extracted=1 — for live sessions, proxy to the runner's in-memory VT
	// (full scrollback history + visible screen as ANSI bytes). This is always
	// better than file extraction: the VT has up to 50,000 rendered lines, while
	// file extraction only works when the file has CSI_3J blocks (which only
	// appear after a reconnect). For dead sessions, fall back to file-based
	// extraction (ExtractBytes on the raw PTY file).
	if r.URL.Query().Get("extracted") == "1" {
		if sess, ok := sessions.Get(sessionID); ok && sess.Alive && sess.SocketPath != "" {
			if rendered, err := fetchRenderedScrollback(sess.SocketPath); err == nil {
				_, _ = w.Write(rendered)
				return
			}
			// Runner unreachable — fall through to file extraction.
		}
		raw, err := io.ReadAll(rc)
		if err != nil {
			log.Printf("scrollback: read %s: %v", sessionID, err)
			writeError(w, http.StatusInternalServerError, "internal", "scrollback read failed")
			return
		}
		extracted := scrollback.ExtractBytes(raw)
		_, _ = w.Write(extracted)
		return
	}

	// Raw stream — io.Copy so we never buffer the whole file.
	// A mid-stream client disconnect (e.g. user closed the tab)
	// surfaces as a Copy error and is not actionable; nothing to log.
	_, _ = io.Copy(w, rc)
}

// fetchRenderedScrollback dials the runner's Unix socket and calls
// GET /scrollback/rendered, which returns renderScreen() as ANSI bytes:
// the full in-memory scrollback followed by the visible screen.
// Used by scrollbackBrokerHandler to serve live sessions via ?extracted=1.
func fetchRenderedScrollback(socketPath string) ([]byte, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", socketPath, 2*time.Second)
			},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get("http://unix/scrollback/rendered")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runner returned %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
