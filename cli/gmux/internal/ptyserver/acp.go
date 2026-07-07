package ptyserver

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/gmuxapp/gmux/cli/gmux/internal/acp"
	"nhooyr.io/websocket"
)

// acpHub synthesizes the ACP conversation stream (ADR 0021) for one session.
//
// It holds ONLY the in-memory unwritten tail — the in-flight partial assistant
// message and its accumulated text — and forgets it the moment pi finalizes
// the message to JSONL (message_end). Durable history lives in the JSONL file
// the runner already owns; the daemon holds no conversation content
// (ADR 0011). This mirrors the PTY ring-buffer + persistent-scrollback split
// (ADR 0016): a small live buffer for what hasn't been persisted, disk for the
// rest.
//
// The hub fans token deltas out to any number of /acp WebSocket subscribers.
// Sends are non-blocking with a buffered channel; a slow client drops frames
// rather than stalling the ingest path (backpressure note in the PR).
type acpHub struct {
	sessionID string

	mu   sync.Mutex
	subs map[chan acp.Notification]struct{}

	// In-memory unwritten tail: the current partial assistant message.
	// tailActive is false between turns (nothing unflushed).
	tailActive bool
	tailMsgID  string
	tailText   string
}

func newACPHub(sessionID string) *acpHub {
	return &acpHub{
		sessionID: sessionID,
		subs:      make(map[chan acp.Notification]struct{}),
	}
}

// acpIngest is the one-way payload the read-only pi extension POSTs to
// /acp/ingest. It is deliberately tiny: begin a message, append a token
// delta, or finalize. See pi-ext.mjs.
type acpIngest struct {
	Op        string `json:"op"`                  // "message_start" | "chunk" | "message_end"
	MessageID string `json:"messageId,omitempty"` // stable id for the in-flight message
	Delta     string `json:"delta,omitempty"`     // token-level text (op "chunk")
}

// ingest applies one extension event: it updates the unwritten tail and, for a
// chunk, broadcasts an agent_message_chunk to live subscribers.
func (h *acpHub) ingest(ev acpIngest) {
	switch ev.Op {
	case "message_start":
		h.mu.Lock()
		h.tailActive = true
		h.tailMsgID = ev.MessageID
		h.tailText = ""
		h.mu.Unlock()
	case "chunk":
		h.mu.Lock()
		if !h.tailActive {
			// A chunk with no preceding message_start (e.g. extension loaded
			// mid-turn): open a tail implicitly so nothing is lost.
			h.tailActive = true
			h.tailMsgID = ev.MessageID
		}
		h.tailText += ev.Delta
		msgID := h.tailMsgID
		h.mu.Unlock()
		if note, err := acp.NewAgentMessageChunk(h.sessionID, msgID, ev.Delta); err == nil {
			h.broadcast(note)
		}
	case "message_end":
		// pi has appended the finalized message to JSONL; forget the tail so
		// session/load reads it from disk, not from memory (ADR 0011/0016).
		h.mu.Lock()
		h.tailActive = false
		h.tailMsgID = ""
		h.tailText = ""
		h.mu.Unlock()
	}
}

// snapshot builds the session/load history: durable JSONL messages plus the
// in-memory unwritten tail (the current partial assistant message, if any).
// Returns the frame under the same lock that registers the subscriber (caller
// coordinates) so no live delta can slip in before the snapshot.
func (h *acpHub) snapshotLocked(convFile string) (acp.Notification, error) {
	msgs, _ := acp.LoadHistory(convFile) // best-effort; empty history is fine
	if h.tailActive && h.tailText != "" {
		msgs = append(msgs, acp.Message{
			Role:      "assistant",
			MessageID: h.tailMsgID, // lets a mid-turn joiner keep appending live deltas
			Content:   []acp.ContentBlock{acp.TextBlock(h.tailText)},
		})
	}
	return acp.NewLoad(h.sessionID, msgs)
}

// attach atomically builds the session/load snapshot and registers a live
// subscriber under the same lock, so no session/update delta can be broadcast
// in the gap between reading the snapshot and subscribing (the snapshot-then-
// stream ordering guarantee, ADR 0004/0021).
func (h *acpHub) attach(convFile string) (acp.Notification, chan acp.Notification, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	snapshot, err := h.snapshotLocked(convFile)
	ch := h.addSubLocked()
	return snapshot, ch, err
}

// addSubLocked registers and returns a new buffered subscriber, sized so brief
// frontend stalls don't block ingest. Caller holds mu.
func (h *acpHub) addSubLocked() chan acp.Notification {
	ch := make(chan acp.Notification, 256)
	h.subs[ch] = struct{}{}
	return ch
}

func (h *acpHub) unsubscribe(ch chan acp.Notification) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// broadcast fans a notification to all subscribers, dropping frames for any
// client whose buffer is full (backpressure: the wire is token-chatty; a
// stalled renderer must not stall ingest).
func (h *acpHub) broadcast(note acp.Notification) {
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- note:
		default:
		}
	}
	h.mu.Unlock()
}

// handleACPIngest is the one-way sink for the read-only pi extension's token
// deltas. Fire-and-forget: like /hook/event, it never pushes back on pi.
func (s *Server) handleACPIngest(w http.ResponseWriter, r *http.Request) {
	if s.acp == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var ev acpIngest
	if err := json.NewDecoder(io.LimitReader(r.Body, 512*1024)).Decode(&ev); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.acp.ingest(ev)
	w.WriteHeader(http.StatusNoContent)
}

// handleACP serves the ACP conversation WebSocket: snapshot-then-stream,
// mirroring the PTY attach (ADR 0004). The first frame is the session/load
// history snapshot; subsequent frames are live session/update notifications.
// Frames are JSON text messages (JSON-RPC 2.0 objects).
func (s *Server) handleACP(w http.ResponseWriter, r *http.Request) {
	if s.acp == nil {
		http.Error(w, "acp not available", http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // local Unix socket / authed upstream
	})
	if err != nil {
		log.Printf("ptyserver: acp ws accept: %v", err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer conn.Close(websocket.StatusNormalClosure, "")

	convFile := ""
	if s.state != nil {
		convFile = s.state.ConversationFileSnapshot()
	}

	snapshot, ch, snapErr := s.acp.attach(convFile)
	defer s.acp.unsubscribe(ch)

	if snapErr == nil {
		if data, err := json.Marshal(snapshot); err == nil {
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
	}

	// Drain client reads (this slice has no client→server messages, but we must
	// read to observe close) while we push notifications.
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case note, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(note)
			if err != nil {
				continue
			}
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
	}
}
