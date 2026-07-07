package ptyserver

import (
	"encoding/json"
	"testing"

	"github.com/gmuxapp/gmux/cli/gmux/internal/acp"
)

func TestACPHubBroadcastsChunks(t *testing.T) {
	h := newACPHub("sess1")
	_, ch, _ := h.attach("") // subscribe to the live stream

	h.ingest(acpIngest{Op: "message_start", MessageID: "m1"})
	h.ingest(acpIngest{Op: "chunk", MessageID: "m1", Delta: "Hel"})
	h.ingest(acpIngest{Op: "chunk", MessageID: "m1", Delta: "lo"})

	got := drainText(t, ch, 2)
	if got != "Hello" {
		t.Fatalf("broadcast deltas = %q, want %q", got, "Hello")
	}
}

func TestACPHubBroadcastsThinkingAsThoughtChunk(t *testing.T) {
	h := newACPHub("sess1")
	_, ch, _ := h.attach("")

	h.ingest(acpIngest{Op: "message_start", MessageID: "m1"})
	h.ingest(acpIngest{Op: "thinking_chunk", MessageID: "m1", Delta: "hmm"})

	note := <-ch
	var p acp.UpdateParams
	if err := json.Unmarshal(note.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.Update.SessionUpdate != acp.UpdateAgentThoughtChunk {
		t.Errorf("sessionUpdate = %q, want %q", p.Update.SessionUpdate, acp.UpdateAgentThoughtChunk)
	}
	if p.Update.Content.Type != acp.ContentTypeThinking || p.Update.Content.Text != "hmm" {
		t.Errorf("thought content = %+v", p.Update.Content)
	}
}

// Interleaved thinking then text within one message must snapshot as two
// ordered blocks (thinking first, text second), each coalesced.
func TestACPHubTailKeepsThinkingAndTextInOrder(t *testing.T) {
	h := newACPHub("sess1")
	h.ingest(acpIngest{Op: "message_start", MessageID: "m1"})
	h.ingest(acpIngest{Op: "thinking_chunk", MessageID: "m1", Delta: "think"})
	h.ingest(acpIngest{Op: "thinking_chunk", MessageID: "m1", Delta: "ing"})
	h.ingest(acpIngest{Op: "chunk", MessageID: "m1", Delta: "ans"})
	h.ingest(acpIngest{Op: "chunk", MessageID: "m1", Delta: "wer"})

	h.mu.Lock()
	note, _ := h.snapshotLocked("")
	h.mu.Unlock()
	var p acp.LoadParams
	if err := json.Unmarshal(note.Params, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Messages) != 1 || len(p.Messages[0].Content) != 2 {
		t.Fatalf("snapshot = %+v", p.Messages)
	}
	c := p.Messages[0].Content
	if c[0].Type != acp.ContentTypeThinking || c[0].Text != "thinking" {
		t.Errorf("block0 = %+v", c[0])
	}
	if c[1].Type != acp.ContentTypeText || c[1].Text != "answer" {
		t.Errorf("block1 = %+v", c[1])
	}
}

func TestACPHubSnapshotIncludesUnwrittenTail(t *testing.T) {
	h := newACPHub("sess1")
	h.ingest(acpIngest{Op: "message_start", MessageID: "m1"})
	h.ingest(acpIngest{Op: "chunk", MessageID: "m1", Delta: "partial"})

	h.mu.Lock()
	note, err := h.snapshotLocked("") // no JSONL; only the in-mem tail
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var p acp.LoadParams
	if err := json.Unmarshal(note.Params, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != "assistant" {
		t.Fatalf("snapshot messages = %+v", p.Messages)
	}
	if p.Messages[0].Content[0].Text != "partial" {
		t.Errorf("tail text = %q", p.Messages[0].Content[0].Text)
	}
	// The tail must carry its streaming messageId so a mid-turn joiner keeps
	// appending subsequent deltas to the same message rather than duplicating it.
	if p.Messages[0].MessageID != "m1" {
		t.Errorf("tail messageId = %q, want m1", p.Messages[0].MessageID)
	}
}

func TestACPHubForgetsTailAfterMessageEnd(t *testing.T) {
	h := newACPHub("sess1")
	h.ingest(acpIngest{Op: "message_start", MessageID: "m1"})
	h.ingest(acpIngest{Op: "chunk", MessageID: "m1", Delta: "done"})
	h.ingest(acpIngest{Op: "message_end", MessageID: "m1"})

	h.mu.Lock()
	note, _ := h.snapshotLocked("") // JSONL now owns it; memory forgot it
	h.mu.Unlock()
	var p acp.LoadParams
	_ = json.Unmarshal(note.Params, &p)
	if len(p.Messages) != 0 {
		t.Fatalf("tail should be forgotten after message_end, got %+v", p.Messages)
	}
}

// drainText reads n notifications and concatenates their text deltas.
func drainText(t *testing.T, ch chan acp.Notification, n int) string {
	t.Helper()
	out := ""
	for i := 0; i < n; i++ {
		note := <-ch
		var p acp.UpdateParams
		if err := json.Unmarshal(note.Params, &p); err != nil {
			t.Fatal(err)
		}
		out += p.Update.Content.Text
	}
	return out
}
