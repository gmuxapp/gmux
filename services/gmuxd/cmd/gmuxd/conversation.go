package main

import (
	"bytes"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// conversationHandler serves GET /v1/sessions/{id}/conversation: a
// clean markdown transcript reconstructed from the session's
// conversation file (the structured file the agent itself writes, e.g.
// pi's JSONL), rather than the PTY rendering of its TUI. This is what
// the default `gmux tail` prints for conversation-backed sessions.
//
// The daemon stays content-free (ADR 0011): nothing here is cached or
// stored — the adapter re-reads the conversation file per request, the
// same way the scrollback broker re-reads the tee. Snapshot semantics
// mirror `gmux tail`: the durable file only, so an in-flight partial
// assistant message (not yet flushed by the agent) is not included.
//
// Optional ?tail=<N> trims to the last N messages — the conversation
// analog of scrollback's last-N-lines. Absent means the whole
// transcript; non-positive or non-numeric N is a 400 (same contract as
// the scrollback broker).
//
// Status code semantics:
//
//   - 405 non-GET: the transcript is read-only.
//   - 404 "not_found": session ID isn't in the store (peer sessions
//     were already forwarded to the owning gmuxd before this handler).
//   - 404 "no_conversation": the session exists but has no renderable
//     conversation — no conversation file attributed, the adapter
//     doesn't implement ConversationRenderer, the file was deleted,
//     or it contains no messages yet. The distinct code is the CLI's
//     signal to fall back to PTY scrollback; a bare not_found would
//     make `gmux tail` misreport the session as missing.
//   - 500 on read/parse IO errors other than file-gone.
func conversationHandler(w http.ResponseWriter, r *http.Request, sessionID string, sessions *store.Store) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}
	sess, ok := sessions.Get(sessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}

	tailN := 0
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "tail must be a positive integer")
			return
		}
		tailN = n
	}

	if sess.ConversationRef == "" {
		writeError(w, http.StatusNotFound, "no_conversation", "session has no conversation")
		return
	}
	a := adapters.FindByAdapter(sess.Adapter)
	if a == nil {
		writeError(w, http.StatusNotFound, "no_conversation", "session has no adapter")
		return
	}
	renderer, ok := a.(adapter.ConversationRenderer)
	if !ok {
		writeError(w, http.StatusNotFound, "no_conversation", "adapter does not render conversations")
		return
	}

	msgs, err := renderer.RenderConversation(sess.ConversationRef)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The conversation was deleted (or its storage unmounted)
			// after attribution. Scrollback may still exist; let the CLI
			// fall back rather than erroring a tail that used to work.
			writeError(w, http.StatusNotFound, "no_conversation", "conversation is gone")
			return
		}
		log.Printf("conversation: %s: %v", sessionID, err)
		writeError(w, http.StatusInternalServerError, "internal", "conversation render failed")
		return
	}
	if len(msgs) == 0 {
		// A conversation with no renderable messages yet (fresh session:
		// header line only). Falling back to scrollback shows the agent's
		// startup screen, which is strictly more useful than an empty
		// 200 body.
		writeError(w, http.StatusNotFound, "no_conversation", "conversation has no messages yet")
		return
	}
	if tailN > 0 && len(msgs) > tailN {
		msgs = msgs[len(msgs)-tailN:]
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(formatConversationMarkdown(msgs))
}

// formatConversationMarkdown renders messages as a markdown transcript:
// a "## <Role>" heading per message, bodies separated by blank lines.
// The heading format is daemon-owned so every adapter's transcript
// reads the same; adapters own only per-message content.
func formatConversationMarkdown(msgs []adapter.ConversationMessage) []byte {
	var b bytes.Buffer
	for i, m := range msgs {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("## ")
		b.WriteString(roleHeading(m.Role))
		b.WriteString("\n\n")
		b.WriteString(strings.TrimRight(m.Text, "\n"))
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// roleHeading maps an adapter-reported role to its transcript heading.
func roleHeading(role string) string {
	switch role {
	case "user":
		return "User"
	case "assistant":
		return "Assistant"
	case "":
		return "Message"
	default:
		return strings.ToUpper(role[:1]) + role[1:]
	}
}
