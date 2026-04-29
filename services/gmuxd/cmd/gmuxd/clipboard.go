package main

import (
	"errors"
	"io"
	"net/http"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/clipfile"
)

// MaxClipboardBytes caps the request body for clipboard uploads.
// Mirrored on the gmux-web side for immediate UX feedback; this is the
// safety floor enforced server-side regardless of client.
//
// Sized to comfortably fit screenshots and short video clips (typical
// PNG screenshot is well under 5MB), while flagging accidents like
// pasting a forgotten copied video early instead of after a long
// upload. Raise on demand; raising is non-breaking.
const MaxClipboardBytes = 10 * 1024 * 1024

// clipboardHandler returns an http.Handler that materializes the
// request body as a file via writer and responds with the absolute
// path. Caller is responsible for routing (path parsing, peer
// forwarding, session lookup) and for not invoking this for sessions
// that aren't owned locally.
func clipboardHandler(writer clipfile.Writer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
			return
		}

		// Cap body before reading. http.MaxBytesReader returns an error
		// of type *http.MaxBytesError once the limit is exceeded; we
		// distinguish "too large" from generic read failures so the
		// client gets an actionable error.
		r.Body = http.MaxBytesReader(w, r.Body, MaxClipboardBytes)
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "too_large",
					"clipboard payload exceeds 10MB limit")
				return
			}
			writeError(w, http.StatusBadRequest, "read_failed", err.Error())
			return
		}
		if len(payload) == 0 {
			writeError(w, http.StatusBadRequest, "empty_body",
				"clipboard payload is empty")
			return
		}

		path, err := writer.Write(payload, r.Header.Get("Content-Type"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "write_failed", err.Error())
			return
		}

		writeJSON(w, map[string]any{
			"ok":   true,
			"data": map[string]any{"path": path},
		})
	})
}
