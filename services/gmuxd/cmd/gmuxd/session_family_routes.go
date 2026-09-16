package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// PROTO (3.0 world split): the two read endpoints the roots-only world state
// needs — a single row (with its family spine) and a paginated, newest-first
// listing of a session's children. Both are cut from the fanout's CURRENT
// memo, not store-direct: the page names the epoch it was cut at so a
// scoped delta subscription can chain onto it, and that epoch must be one
// the fanout actually published. (REST /v1/sessions stays store-direct.)
//
// For `id@peer` rows the hub forwards to the owning daemon and re-namespaces
// the answer (ids, parent references, peer stamp). A 2.1 spoke does not have
// these routes; the hub then answers from its own projection, which for a
// 2.1 spoke still contains the children (mixed-version degrade).

type sessionDetail struct {
	Session   wire.Session   `json:"session"`
	Ancestors []wire.Session `json:"ancestors,omitempty"`
	Epoch     uint64         `json:"epoch,omitempty"`
	BootID    string         `json:"boot_id,omitempty"`
}

func handleSessionDetail(w http.ResponseWriter, r *http.Request, fanout *sseFanout, peerManager *peering.Manager, sessionID string) {
	if peerManager != nil {
		if peer, originalID := peerManager.FindPeer(sessionID); peer != nil {
			if forwardPeerDetail(w, r, peer, originalID, fanout) {
				return
			}
		}
	}
	memo, _ := fanout.CurrentMemo()
	if memo == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no snapshot yet")
		return
	}
	rows, family := memo.Annotated()
	var found *wire.Session
	for i := range rows {
		if rows[i].ID == sessionID {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	out := sessionDetail{Session: *found, Epoch: memo.epoch, BootID: fanout.BootID()}
	if r.URL.Query().Get("ancestors") == "1" {
		out.Ancestors = wire.Ancestors(rows, family, sessionID)
	}
	writeJSON(w, map[string]any{"ok": true, "data": out})
}

func handleSessionChildren(w http.ResponseWriter, r *http.Request, fanout *sseFanout, peerManager *peering.Manager, sessionID string) {
	q := r.URL.Query()
	limit := 100
	if v, err := strconv.Atoi(q.Get("limit")); err == nil {
		limit = v
	}
	cursor := q.Get("cursor")
	if _, _, ok := wire.DecodeCursor(cursor); !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor")
		return
	}
	descendants := q.Get("descendants") == "1"
	if peerManager != nil {
		if peer, originalID := peerManager.FindPeer(sessionID); peer != nil {
			if forwardPeerChildren(w, r, peer, originalID, fanout) {
				return
			}
		}
	}
	memo, _ := fanout.CurrentMemo()
	if memo == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no snapshot yet")
		return
	}
	page, ok := memo.ChildrenPage(sessionID, descendants, cursor, limit)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	page.Epoch = memo.epoch
	page.BootID = fanout.BootID()
	writeJSON(w, map[string]any{"ok": true, "data": page})
}

// forwardPeerChildren asks the owning daemon for the page and re-namespaces
// it. Returns false when the spoke does not speak the route (a 2.1 daemon):
// the caller then serves from the local projection.
func forwardPeerChildren(w http.ResponseWriter, r *http.Request, peer *peering.Peer, originalID string, fanout *sseFanout) bool {
	path := "/v1/sessions/" + originalID + "/children"
	if raw := r.URL.RawQuery; raw != "" {
		path += "?" + raw
	}
	body, status, err := peer.FetchJSON(r.Context(), path)
	if err != nil {
		log.Printf("children: %s: forward: %v", peer.Config.Name, err)
		writeError(w, http.StatusBadGateway, "unavailable", "peer unavailable")
		return true
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		// Either the spoke predates the route or the id is gone there; the
		// local projection answers both cases correctly (empty or 404).
		return false
	}
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return true
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			SessionID   string                      `json:"session_id"`
			Rows        []peering.SessionProjection `json:"rows"`
			NextCursor  string                      `json:"next_cursor,omitempty"`
			Total       int                         `json:"total"`
			Descendants bool                        `json:"descendants,omitempty"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || !envelope.OK {
		writeError(w, http.StatusBadGateway, "unavailable", "bad peer response")
		return true
	}
	rows := make([]peering.SessionProjection, 0, len(envelope.Data.Rows))
	for _, row := range envelope.Data.Rows {
		rows = append(rows, peer.NamespaceProjection(row))
	}
	// The page is stamped with the HUB's epoch: a scope registered on the
	// hub chains from hub epochs. (A hub does not relay scopes to spokes in
	// this prototype; the browser falls back to count-triggered refetch for
	// peer subtrees — see the report.)
	_, epoch := fanout.CurrentMemo()
	writeJSON(w, map[string]any{"ok": true, "data": map[string]any{
		"session_id":  peering.NamespaceID(envelope.Data.SessionID, peer.Config.Name),
		"rows":        rows,
		"next_cursor": envelope.Data.NextCursor,
		"total":       envelope.Data.Total,
		"descendants": envelope.Data.Descendants,
		"epoch":       epoch,
		"boot_id":     fanout.BootID(),
		"peer":        peer.Config.Name,
	}})
	return true
}

func forwardPeerDetail(w http.ResponseWriter, r *http.Request, peer *peering.Peer, originalID string, fanout *sseFanout) bool {
	path := "/v1/sessions/" + originalID
	if raw := r.URL.RawQuery; raw != "" {
		path += "?" + raw
	}
	body, status, err := peer.FetchJSON(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusBadGateway, "unavailable", "peer unavailable")
		return true
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return false
	}
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return true
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Session   peering.SessionProjection   `json:"session"`
			Ancestors []peering.SessionProjection `json:"ancestors"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || !envelope.OK {
		writeError(w, http.StatusBadGateway, "unavailable", "bad peer response")
		return true
	}
	ancestors := make([]peering.SessionProjection, 0, len(envelope.Data.Ancestors))
	for _, row := range envelope.Data.Ancestors {
		ancestors = append(ancestors, peer.NamespaceProjection(row))
	}
	_, epoch := fanout.CurrentMemo()
	writeJSON(w, map[string]any{"ok": true, "data": map[string]any{
		"session":   peer.NamespaceProjection(envelope.Data.Session),
		"ancestors": ancestors,
		"epoch":     epoch,
		"boot_id":   fanout.BootID(),
	}})
	return true
}
