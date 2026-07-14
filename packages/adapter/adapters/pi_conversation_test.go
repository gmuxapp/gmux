package adapters

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// TestRenderConversationTranscript is the core contract of the pi
// ConversationRenderer: a JSONL file yields the user/assistant exchange
// in order, with tool calls compacted to one-liners and internal noise
// (thinking blocks, toolResult messages, non-message entries) dropped.
// This is what `gmux tail` prints by default for pi sessions.
func TestRenderConversationTranscript(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc-123","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"model_change","id":"m1","timestamp":"2026-03-15T10:00:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"Fix the auth bug"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"let me look around"},{"type":"text","text":"Looking now."},{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"go test ./..."}}]}}`,
		`{"type":"message","id":"r1","message":{"role":"toolResult","toolCallId":"t1","toolName":"bash","content":[{"type":"text","text":"ok\t0.5s"}]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","content":[{"type":"text","text":"All green."}]}}`,
	)

	msgs, err := NewPi().RenderConversation(path)
	if err != nil {
		t.Fatal(err)
	}

	want := []adapter.ConversationMessage{
		{Role: "user", Text: "Fix the auth bug"},
		{Role: "assistant", Text: "Looking now.\n\n[tool] bash {\"command\":\"go test ./...\"}"},
		{Role: "assistant", Text: "All green."},
	}
	if len(msgs) != len(want) {
		t.Fatalf("message count: want %d, got %d (%+v)", len(want), len(msgs), msgs)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Errorf("msg[%d]: want %+v, got %+v", i, want[i], msgs[i])
		}
	}
}

// TestRenderConversationSkipsThinkingOnlyTurn: an assistant message
// consisting solely of thinking blocks renders to nothing and must be
// omitted entirely — otherwise the transcript would show empty
// "## Assistant" stubs for every internal deliberation.
func TestRenderConversationSkipsThinkingOnlyTurn(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
	)
	msgs, err := NewPi().RenderConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("want only the user message, got %+v", msgs)
	}
}

// TestRenderConversationStringContent covers pi's older plain-string
// content encoding (also produced by some injected messages). It must
// render as the message body, not be dropped as unparseable.
func TestRenderConversationStringContent(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":"plain string prompt"}}`,
	)
	msgs, err := NewPi().RenderConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "plain string prompt" {
		t.Fatalf("want plain string content, got %+v", msgs)
	}
}

// TestRenderConversationImagePlaceholder: image blocks carry base64
// payloads that must never hit the transcript; they render as a
// placeholder token instead.
func TestRenderConversationImagePlaceholder(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"image","data":"aGVsbG8=","mimeType":"image/png"},{"type":"text","text":"what is this?"}]}}`,
	)
	msgs, err := NewPi().RenderConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "[image]\n\nwhat is this?" {
		t.Fatalf("want image placeholder + text, got %+v", msgs)
	}
}

// TestRenderConversationSkipsMalformedTailLine: pi appends to the JSONL
// live, so a read can catch the final line mid-write. A torn line must
// be skipped, not fail the whole render — `gmux tail` on a live session
// is exactly when this happens.
func TestRenderConversationSkipsMalformedTailLine(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","con`, // torn write
	)
	msgs, err := NewPi().RenderConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hi" {
		t.Fatalf("want the intact message only, got %+v", msgs)
	}
}

// TestRenderConversationMissingFile: the error must satisfy
// fs.ErrNotExist so gmuxd can map "file deleted" to the scrollback
// fallback rather than a 500.
func TestRenderConversationMissingFile(t *testing.T) {
	_, err := NewPi().RenderConversation("/nonexistent/conv.jsonl")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want fs.ErrNotExist, got %v", err)
	}
}

// TestFormatPiToolCallTruncatesArgs: tool arguments (edit bodies, long
// shell commands) are capped so tool lines stay one-line context, and
// no markdown wrapping is added around the arbitrary argument bytes.
func TestFormatPiToolCallTruncatesArgs(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := formatPiToolCall("edit", []byte(`{"text":"`+long+`"}`))
	if !strings.HasPrefix(got, "[tool] edit {\"text\":\"xxx") {
		t.Fatalf("prefix: got %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("want truncation ellipsis, got %q", got)
	}
	if n := len([]rune(got)); n > len("[tool] edit ")+maxToolArgChars+1 {
		t.Fatalf("rendered tool call too long: %d runes: %q", n, got)
	}

	// No-arg and empty-arg calls render bare.
	if got := formatPiToolCall("ls", nil); got != "[tool] ls" {
		t.Fatalf("nil args: got %q", got)
	}
	if got := formatPiToolCall("ls", []byte(`{}`)); got != "[tool] ls" {
		t.Fatalf("empty args: got %q", got)
	}
}
