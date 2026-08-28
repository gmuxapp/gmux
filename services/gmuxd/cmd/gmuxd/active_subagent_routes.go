package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

const (
	codeSubagentLimitReached       = "subagent_limit_reached"
	codeInvalidSubagentReservation = "invalid_subagent_reservation"
)

// registerActiveSubagentRoutes registers the launch-admission reservation
// endpoints. These are internal daemon↔CLI plumbing for the active-subagent
// budget: they happen to live under /v1/ but are NOT part of the covenanted
// HTTP surface (see the website's Interface stability page). Their request
// shape, token lifecycle, and error codes may change in any release; external
// callers must not depend on them.
func registerActiveSubagentRoutes(mux *http.ServeMux, coord *sessioncoord.Coordinator) {
	mux.HandleFunc("POST /v1/agent-launch-reservations", func(w http.ResponseWriter, r *http.Request) {
		handleActiveSubagentReservation(w, r, coord)
	})
	mux.HandleFunc("DELETE /v1/agent-launch-reservations/{token}", func(w http.ResponseWriter, r *http.Request) {
		handleActiveSubagentReservationRelease(w, r, coord, r.PathValue("token"))
	})
}

func handleActiveSubagentReservation(w http.ResponseWriter, r *http.Request, coord *sessioncoord.Coordinator) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(body) > 4096 {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var wire struct {
		ParentSessionID *string `json:"parent_session_id"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	var parent *centralstore.SessionID
	if wire.ParentSessionID != nil {
		if *wire.ParentSessionID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "parent_session_id must be a session id or null")
			return
		}
		id := centralstore.SessionID(*wire.ParentSessionID)
		parent = &id
	}
	reservation, err := coord.ReserveActiveSubagent(r.Context(), parent)
	if err != nil {
		var limit *sessioncoord.SubagentLimitError
		if errors.As(err, &limit) {
			writeError(w, http.StatusTooManyRequests, codeSubagentLimitReached, formatSubagentLimitMessage(limit))
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "data": map[string]any{
		"token": reservation.Token, "root_session_id": reservation.Root,
		"depth": reservation.Depth, "active": reservation.Active, "limit": reservation.Limit,
		"expires_at": reservation.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}})
}

func formatSubagentLimitMessage(limit *sessioncoord.SubagentLimitError) string {
	if limit.Limit == 0 {
		return fmt.Sprintf("subagent limit reached at depth %d for root %s: sessions at this depth may not spawn subagents on this host; ask a human to extend the limit or promote this subtree to its own root", limit.Depth, limit.Root)
	}
	return fmt.Sprintf("subagent limit reached at depth %d for root %s: %d of %d autonomous subagents at this depth; run 'gmux ls' to see who holds the slots, dismiss finished sessions, or ask a human to raise this host's limit or promote this subtree to its own root", limit.Depth, limit.Root, limit.Active, limit.Limit)
}

func handleActiveSubagentReservationRelease(w http.ResponseWriter, r *http.Request, coord *sessioncoord.Coordinator, token string) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}
	if token == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "reservation token required")
		return
	}
	coord.ReleaseActiveSubagentReservation(token)
	w.WriteHeader(http.StatusNoContent)
}
