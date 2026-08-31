package main

import (
	"net/http"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/conversations"
)

// handleConversationLookup serves GET /v1/conversations/{adapter}/{slug}.
//
// While the startup snapshot is still running, a lookup miss is ambiguous —
// the conversation may simply not have been scanned yet — so the handler
// answers 503 "indexing" with Retry-After instead of a false 404. Completeness
// is read BEFORE the lookup: only a miss against an index that was already
// complete when the request started is an authoritative 404. Reading it after
// the miss could race the final adapter's upsert + completeness flip and
// 404 a conversation that is in the index by the time the response is
// written; with this order, the worst case is one extra retryable 503.
func handleConversationLookup(convIndex *conversations.Index) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adapterName := r.PathValue("adapter")
		slug := r.PathValue("slug")
		complete := convIndex.SnapshotComplete()
		info, ok := convIndex.Lookup(adapterName, slug)
		if !ok {
			if !complete {
				w.Header().Set("Retry-After", "2")
				writeError(w, http.StatusServiceUnavailable, "indexing", "conversation index is still loading; retry shortly")
				return
			}
			writeError(w, http.StatusNotFound, "not_found", "conversation not found")
			return
		}
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"slug": info.Slug, "adapter": info.Adapter, "title": info.Title, "cwd": info.Cwd, "resume_command": info.ResumeCommand, "created": info.Created}})
	}
}

// conversationIndexHealth is the /v1/health "conversation_index" payload:
// transport readiness must not imply complete conversation discovery, so the
// scan's state is reported explicitly.
func conversationIndexHealth(convIndex *conversations.Index) map[string]any {
	status := "indexing"
	if convIndex.SnapshotComplete() {
		status = "ready"
	}
	return map[string]any{"status": status, "indexed": convIndex.Count()}
}
