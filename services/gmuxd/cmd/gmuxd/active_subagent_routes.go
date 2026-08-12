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
			message := fmt.Sprintf("subagent limit reached for root %s: %d of %d active subagents; run 'gmux ls' to inspect this host's sessions", limit.Root, limit.Active, limit.Limit)
			writeError(w, http.StatusTooManyRequests, codeSubagentLimitReached, message)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "data": map[string]any{
		"token": reservation.Token, "root_session_id": reservation.Root,
		"active": reservation.Active, "limit": reservation.Limit,
		"expires_at": reservation.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}})
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
