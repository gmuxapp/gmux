package ringbuf

import (
	"bytes"
	"testing"
)

func TestTermWriter_PlainText(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("hello world\n"))
	got := tw.Snapshot()
	if !bytes.Equal(got, []byte("hello world\n")) {
		t.Errorf("got %q, want %q", got, "hello world\n")
	}
}

func TestTermWriter_PartialLine(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("no newline"))
	got := tw.Snapshot()
	if !bytes.Equal(got, []byte("no newline")) {
		t.Errorf("got %q, want %q", got, "no newline")
	}
}

func TestTermWriter_CRLF(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("line one\r\nline two\r\n"))
	got := tw.Snapshot()
	if !bytes.Equal(got, []byte("line one\r\nline two\r\n")) {
		t.Errorf("got %q, want %q", got, "line one\r\nline two\r\n")
	}
}

func TestTermWriter_CRLFSplitAcrossChunks(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("hello\r"))
	tw.Write([]byte("\nnext"))
	got := tw.Snapshot()
	if !bytes.Equal(got, []byte("hello\r\nnext")) {
		t.Errorf("got %q, want %q", got, "hello\r\nnext")
	}
}

func TestTermWriter_BareCR_Collapses(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("old content\rnew stuff"))
	got := tw.Snapshot()
	if !bytes.Equal(got, []byte("new stuff")) {
		t.Errorf("got %q, want %q", got, "new stuff")
	}
}

func TestTermWriter_ConsecutiveCRs(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("aaa\r\r\rbbb"))
	got := tw.Snapshot()
	// Multiple CRs followed by non-LF: bare CR, only final content survives.
	if !bytes.Equal(got, []byte("bbb")) {
		t.Errorf("got %q, want %q", got, "bbb")
	}
}

func TestTermWriter_Spinner(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("⠋ Loading...\r⠙ Loading...\r⠹ Loading...\r⠸ Loading..."))
	got := tw.Snapshot()
	want := "⠸ Loading..."
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_SpinnerThenNewline(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("⠋ Building\r⠙ Building\r✓ Done\n"))
	got := tw.Snapshot()
	want := "✓ Done\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_SpinnerAcrossChunks(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("⠋ Loading..."))
	tw.Write([]byte("\r⠙ Loading..."))
	tw.Write([]byte("\r⠹ Done\n"))
	got := tw.Snapshot()
	want := "⠹ Done\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_BareCR_DeferredThenResolved(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("old stuff\r"))
	// Pending CR, not yet resolved. Snapshot includes it for correct rendering.
	snap := tw.Snapshot()
	if !bytes.Equal(snap, []byte("old stuff\r")) {
		t.Errorf("pending snapshot: got %q, want %q", snap, "old stuff\r")
	}
	// Non-LF byte confirms bare CR: old content is discarded.
	tw.Write([]byte("new"))
	got := tw.Snapshot()
	if !bytes.Equal(got, []byte("new")) {
		t.Errorf("after resolve: got %q, want %q", got, "new")
	}
}

// Clear sequences are passed through as regular content, not used to reset.
// TUI apps (pi, claude) use clears as part of normal rendering; resetting
// would discard meaningful conversation content. WebSocket clients that
// replay the scrollback will process the escape sequences naturally.

func TestTermWriter_ClearScreen_PassedThrough(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("before clear\n"))
	tw.Write([]byte("\x1b[2Jafter clear"))
	got := tw.Snapshot()
	// Both pre- and post-clear content are preserved.
	want := "before clear\n\x1b[2Jafter clear"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_ClearScrollback_PassedThrough(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("line1\nline2\nline3\n"))
	tw.Write([]byte("\x1b[3Jfresh start"))
	got := tw.Snapshot()
	want := "line1\nline2\nline3\n\x1b[3Jfresh start"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_ClearInMiddleOfChunk(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("junk\x1b[2Jgood stuff\n"))
	got := tw.Snapshot()
	// Clear is passed through as content.
	want := "junk\x1b[2Jgood stuff\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_ANSICodesPreserved(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("\x1b[32mgreen text\x1b[0m\n"))
	got := tw.Snapshot()
	want := "\x1b[32mgreen text\x1b[0m\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_CRWithANSISpinner(t *testing.T) {
	tw := NewTermWriter(New(256))
	// Colored spinner frames: each frame re-sets color, so no state is lost.
	tw.Write([]byte("\x1b[32m⠋\x1b[0m Loading...\r\x1b[32m⠙\x1b[0m Loading...\r\x1b[32m⠹\x1b[0m Done!\n"))
	got := tw.Snapshot()
	want := "\x1b[32m⠹\x1b[0m Done!\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_MixedLinesAndSpinners(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("Step 1: compiling\n"))
	tw.Write([]byte("⠋ building\r⠙ building\r⠹ building\r✓ built\n"))
	tw.Write([]byte("Step 2: testing\n"))
	got := tw.Snapshot()
	want := "Step 1: compiling\n✓ built\nStep 2: testing\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_EmptyWrite(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("hello"))
	tw.Write([]byte{})
	tw.Write(nil)
	got := tw.Snapshot()
	if !bytes.Equal(got, []byte("hello")) {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestTermWriter_Reset(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("some data\npartial"))
	tw.Reset()
	if tw.Len() != 0 {
		t.Errorf("expected len 0 after reset, got %d", tw.Len())
	}
	got := tw.Snapshot()
	if len(got) != 0 {
		t.Errorf("expected empty after reset, got %q", got)
	}
}

func TestTermWriter_OversizedLineFlushes(t *testing.T) {
	tw := NewTermWriter(New(256 * 1024)) // 256KB ring buffer

	// Write a line longer than maxPendingLine (64KB) without any newlines.
	big := make([]byte, maxPendingLine+100)
	for i := range big {
		big[i] = 'A'
	}
	tw.Write(big)

	// The pending line should have been flushed to the ring buffer.
	// currentLine holds at most the tail after the flush.
	snap := tw.Snapshot()
	if len(snap) != len(big) {
		t.Errorf("expected snapshot length %d, got %d", len(big), len(snap))
	}

	// After a flush, a subsequent \r can only discard the unflushed tail,
	// not the already-flushed bytes. This is the expected trade-off.
	tw.Write([]byte("\roverwrite"))
	snap = tw.Snapshot()
	if !bytes.HasSuffix(snap, []byte("overwrite")) {
		t.Errorf("expected snapshot to end with 'overwrite', got tail: %q", snap[len(snap)-20:])
	}
	// The flushed bytes are still in the ring buffer.
	if len(snap) < maxPendingLine {
		t.Errorf("expected flushed bytes to persist, snapshot length %d", len(snap))
	}
}

func TestTermWriter_Len(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("abc\ndef"))
	// "abc\n" flushed to ring (4 bytes), "def" pending (3 bytes)
	if tw.Len() != 7 {
		t.Errorf("expected len 7, got %d", tw.Len())
	}
}

// --- PTY onlcr tests: \r\r\n handling ---
// The kernel PTY line discipline (onlcr) converts \n to \r\n. When a
// program writes \r\n, the master side sees \r\r\n. The TermWriter must
// treat any run of \r followed by \n as a CRLF line terminator, not as
// a bare CR that discards line content.

func TestTermWriter_CRCRLF_PreservesContent(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("hello world\r\r\n"))
	got := tw.Snapshot()
	want := "hello world\r\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_CRCRLF_MultipleLines(t *testing.T) {
	tw := NewTermWriter(New(1024))
	tw.Write([]byte("line one\r\r\nline two\r\r\nline three\r\r\n"))
	got := tw.Snapshot()
	want := "line one\r\nline two\r\nline three\r\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_CRCRLF_SplitAcrossChunks(t *testing.T) {
	tw := NewTermWriter(New(256))
	// \r\r\n split: first chunk ends with \r, second starts with \r\n
	tw.Write([]byte("content\r"))
	tw.Write([]byte("\r\n"))
	got := tw.Snapshot()
	want := "content\r\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_CRCRLF_SplitAfterSecondCR(t *testing.T) {
	tw := NewTermWriter(New(256))
	// \r\r\n split: first chunk ends with \r\r, second starts with \n
	tw.Write([]byte("content\r\r"))
	tw.Write([]byte("\n"))
	got := tw.Snapshot()
	want := "content\r\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_TripleCRLF(t *testing.T) {
	tw := NewTermWriter(New(256))
	// Three CRs followed by LF: still a line terminator.
	tw.Write([]byte("hello\r\r\r\n"))
	got := tw.Snapshot()
	want := "hello\r\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_PiLikeOutput(t *testing.T) {
	// Simulates pi's actual output pattern: ANSI-styled lines ending in \r\r\n,
	// with a clear sequence in between renders.
	tw := NewTermWriter(New(4096))
	tw.Write([]byte("\x1b[38;2;102;102;102mversion\x1b[39m v0.62.0\r\r\n"))
	tw.Write([]byte("\x1b[38;2;102;102;102mmodel\x1b[39m claude-haiku-4-5\r\r\n"))
	tw.Write([]byte("\r\r\n"))
	tw.Write([]byte("> say hello\r\r\n"))
	tw.Write([]byte("\r\r\n"))
	tw.Write([]byte("Hello! How can I help you today?\r\r\n"))

	got := tw.Snapshot()
	// All lines should be preserved.
	if !bytes.Contains(got, []byte("version")) {
		t.Errorf("missing 'version' in snapshot: %q", got)
	}
	if !bytes.Contains(got, []byte("say hello")) {
		t.Errorf("missing 'say hello' in snapshot: %q", got)
	}
	if !bytes.Contains(got, []byte("Hello! How can I help you today?")) {
		t.Errorf("missing response in snapshot: %q", got)
	}
}

func TestTermWriter_PiClearThenRedraw(t *testing.T) {
	// Pi does ESC[2J ESC[H ESC[3J then redraws the status bar.
	// The clear sequences should be passed through, preserving pre-clear content.
	tw := NewTermWriter(New(4096))
	tw.Write([]byte("conversation content\r\r\n"))
	tw.Write([]byte("more content\r\r\n"))
	tw.Write([]byte("\x1b[2J\x1b[H\x1b[3J"))
	tw.Write([]byte("status bar\r\r\n"))

	got := tw.Snapshot()
	// Pre-clear content is preserved in the buffer.
	if !bytes.Contains(got, []byte("conversation content")) {
		t.Errorf("pre-clear content lost: %q", got)
	}
	if !bytes.Contains(got, []byte("status bar")) {
		t.Errorf("post-clear content missing: %q", got)
	}
}

func TestTermWriter_SpinnerThenCRCRLF(t *testing.T) {
	// Spinner frames followed by completion with \r\r\n
	tw := NewTermWriter(New(256))
	tw.Write([]byte("⠋ building\r⠙ building\r✓ done\r\r\n"))
	got := tw.Snapshot()
	want := "✓ done\r\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}
