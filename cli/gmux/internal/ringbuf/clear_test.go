package ringbuf

import (
	"bytes"
	"testing"
)

// =============================================================
// Clear screen handling: ESC[2J and ESC[3J should discard prior
// scrollback content so that replay doesn't leave garbage.
// =============================================================

func TestTermWriter_ClearScreen_DiscardsPriorContent(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("line1\nline2\n"))
	tw.Write([]byte("\x1b[2Jnew content\n"))
	got := tw.Snapshot()
	want := "new content\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_ClearScrollback_DiscardsPriorContent(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("line1\nline2\n"))
	tw.Write([]byte("\x1b[3Jfresh\n"))
	got := tw.Snapshot()
	want := "fresh\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_ClearMidChunk(t *testing.T) {
	// Clear in the middle of a single write: only post-clear survives.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("garbage\x1b[2Jgood stuff\n"))
	got := tw.Snapshot()
	want := "good stuff\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_MultipleClears(t *testing.T) {
	// Multiple clears in one chunk: only content after the last clear survives.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("first\n\x1b[2Jsecond\n\x1b[2Jthird\n"))
	got := tw.Snapshot()
	want := "third\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_Clear_DiscardsPendingLine(t *testing.T) {
	// A pending (unflushed) partial line should also be discarded on clear.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("pending partial"))
	tw.Write([]byte("\x1b[2Jnew\n"))
	got := tw.Snapshot()
	want := "new\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_Clear_DiscardsPendingCR(t *testing.T) {
	// A deferred CR from a previous chunk should be discarded by a clear.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("spinner frame\r"))
	// pendingCR is true, currentLine = "spinner frame"
	tw.Write([]byte("\x1b[2Jclean start\n"))
	got := tw.Snapshot()
	want := "clean start\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_ClearAndHome_Pattern(t *testing.T) {
	// Common real-world pattern: ESC[2J ESC[H ESC[3J then new content.
	// The last clear (ESC[3J) is the final discard point.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("old content\n"))
	tw.Write([]byte("\x1b[2J\x1b[H\x1b[3Jstatus bar\n"))
	got := tw.Snapshot()
	want := "status bar\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_ClearResetsFullState(t *testing.T) {
	// After wrapping and then clearing, the buffer should no longer be
	// Full, so Snapshot should not trim the first line.
	tw := NewTermWriter(New(40))
	// Fill past capacity to trigger Full.
	tw.Write([]byte("aaaaaaaaaa\n")) // 11 bytes
	tw.Write([]byte("bbbbbbbbbb\n")) // 11 bytes
	tw.Write([]byte("cccccccccc\n")) // 11 bytes
	tw.Write([]byte("dddddddddd\n")) // 11 bytes (44 total, buffer is 40)

	// Clear resets everything.
	tw.Write([]byte("\x1b[2J"))
	tw.Write([]byte("first\nsecond\n"))

	got := tw.Snapshot()
	want := "first\nsecond\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- Split across chunks ---

func TestTermWriter_ClearSplitAcrossChunks_EscBracket(t *testing.T) {
	// ESC[ at end of one chunk, 2J at start of next.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("old stuff\n"))
	tw.Write([]byte("\x1b["))
	tw.Write([]byte("2Jnew\n"))
	got := tw.Snapshot()
	want := "new\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_ClearSplitAcrossChunks_EscOnly(t *testing.T) {
	// Just ESC at end of one chunk, [2J at start of next.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("old stuff\n"))
	tw.Write([]byte("\x1b"))
	tw.Write([]byte("[2Jnew\n"))
	got := tw.Snapshot()
	want := "new\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_ClearSplitAcrossChunks_EscBracketDigit(t *testing.T) {
	// ESC[2 at end of one chunk, J at start of next.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("old stuff\n"))
	tw.Write([]byte("\x1b[2"))
	tw.Write([]byte("Jnew\n"))
	got := tw.Snapshot()
	want := "new\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_EscBufNonClear_PassedThrough(t *testing.T) {
	// ESC[ held in escBuf, but next chunk completes ESC[1J (not a clear).
	// Content must be preserved, not discarded.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("hello\x1b["))
	tw.Write([]byte("1Jmore\n"))
	got := tw.Snapshot()
	want := "hello\x1b[1Jmore\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_EscBufNonClear_ColorCode(t *testing.T) {
	// ESC[3 held in escBuf, but next chunk completes ESC[32m (color, not clear).
	tw := NewTermWriter(New(256))
	tw.Write([]byte("text\x1b[3"))
	tw.Write([]byte("2mgreen\x1b[0m\n"))
	got := tw.Snapshot()
	want := "text\x1b[32mgreen\x1b[0m\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- Negative tests: non-clear ED modes ---

func TestTermWriter_NonClearEscape_Preserved(t *testing.T) {
	// ESC[0J (erase from cursor down) is NOT a full clear.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("keep this\n\x1b[0Jmore\n"))
	got := tw.Snapshot()
	if !bytes.Contains(got, []byte("keep this")) {
		t.Errorf("ESC[0J should not discard content, got %q", got)
	}
}

func TestTermWriter_NonClearEscape1J_Preserved(t *testing.T) {
	// ESC[1J (erase from cursor up) is NOT a full clear.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("keep this\n\x1b[1Jmore\n"))
	got := tw.Snapshot()
	if !bytes.Contains(got, []byte("keep this")) {
		t.Errorf("ESC[1J should not discard content, got %q", got)
	}
}

func TestTermWriter_DefaultEraseDisplay_Preserved(t *testing.T) {
	// ESC[J (no param, defaults to 0) is NOT a full clear.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("keep this\n\x1b[Jmore\n"))
	got := tw.Snapshot()
	if !bytes.Contains(got, []byte("keep this")) {
		t.Errorf("ESC[J should not discard content, got %q", got)
	}
}

// --- Real-world scenario tests ---

func TestTermWriter_WatchStyleLoop(t *testing.T) {
	// Simulates `watch` command: repeated clear + redraw cycles.
	// Only the last iteration's content should be in the scrollback.
	tw := NewTermWriter(New(4096))

	// First iteration
	tw.Write([]byte("\x1b[H\x1b[2J"))
	tw.Write([]byte("Every 2s: pr-status\n\n"))
	tw.Write([]byte("PR #1: open\nPR #2: merged\n"))

	// Second iteration (watch clears and redraws)
	tw.Write([]byte("\x1b[H\x1b[2J"))
	tw.Write([]byte("Every 2s: pr-status\n\n"))
	tw.Write([]byte("PR #1: merged\nPR #2: merged\nPR #3: open\n"))

	got := tw.Snapshot()
	if bytes.Contains(got, []byte("PR #1: open")) {
		t.Errorf("stale first-iteration content should be gone, got %q", got)
	}
	if !bytes.Contains(got, []byte("PR #1: merged")) {
		t.Errorf("second iteration content missing, got %q", got)
	}
	if !bytes.Contains(got, []byte("PR #3: open")) {
		t.Errorf("second iteration content missing, got %q", got)
	}
}

func TestTermWriter_PiThinkingClear(t *testing.T) {
	// Simulates pi clearing during agent thinking then redrawing.
	tw := NewTermWriter(New(4096))
	tw.Write([]byte("version v0.62.0\r\r\n"))
	tw.Write([]byte("> do something\r\r\n"))
	tw.Write([]byte("thinking...\r\r\n"))
	// Pi clears and redraws with updated content
	tw.Write([]byte("\x1b[2J\x1b[H\x1b[3J"))
	tw.Write([]byte("version v0.62.0\r\r\n"))
	tw.Write([]byte("> do something\r\r\n"))
	tw.Write([]byte("Here is my answer.\r\r\n"))

	got := tw.Snapshot()
	if bytes.Contains(got, []byte("thinking")) {
		t.Errorf("pre-clear 'thinking' should be gone, got %q", got)
	}
	if !bytes.Contains(got, []byte("Here is my answer")) {
		t.Errorf("post-clear answer missing, got %q", got)
	}
}

// =============================================================
// Ring buffer wrapping: snapshot should start at a clean line
// boundary, not mid-line or mid-escape-sequence.
// =============================================================

func TestTermWriter_WrappedBuffer_StartsAtLineBoundary(t *testing.T) {
	// 40-byte ring buffer. Write enough to wrap.
	tw := NewTermWriter(New(40))
	tw.Write([]byte("aaaaaaaaaa\n")) // 11 bytes
	tw.Write([]byte("bbbbbbbbbb\n")) // 11 bytes (total: 22)
	tw.Write([]byte("cccccccccc\n")) // 11 bytes (total: 33)
	tw.Write([]byte("dddddddddd\n")) // 11 bytes (total: 44, wrapped)

	got := tw.Snapshot()
	// 44 bytes into a 40-byte buffer: 4 bytes of "aaaa..." overwritten.
	// Raw snapshot starts with the partial "aaaaaa\n" fragment.
	// After trimming, the first line must be complete.
	firstNewline := bytes.IndexByte(got, '\n')
	if firstNewline < 0 {
		t.Fatalf("no newline in snapshot: %q", got)
	}
	firstLine := got[:firstNewline]
	if len(firstLine) != 10 {
		t.Errorf("first line should be a complete 10-char line, got %q (len %d)", firstLine, len(firstLine))
	}
}

func TestTermWriter_WrappedBuffer_TrimmedEscapeSequence(t *testing.T) {
	// Wrapping cuts through an ANSI-colored line. After trimming, the
	// snapshot should start at the next complete line.
	tw := NewTermWriter(New(40))
	tw.Write([]byte("\x1b[32mgreen\x1b[0m\n"))    // 15 bytes
	tw.Write([]byte("\x1b[31mred\x1b[0m\n"))       // 13 bytes (total: 28)
	tw.Write([]byte("\x1b[34mblue text\x1b[0m\n")) // 19 bytes (total: 47, wrapped)

	got := tw.Snapshot()
	// 47 into 40: first 7 bytes overwritten, leaving 8-byte partial "een\x1b[0m\n".
	// After trimming past that first \n, we should have the last two complete lines.
	want := "\x1b[31mred\x1b[0m\n\x1b[34mblue text\x1b[0m\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_UnwrappedBuffer_NoTrimming(t *testing.T) {
	// When the buffer hasn't wrapped, no trimming should occur.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("line1\nline2\n"))
	got := tw.Snapshot()
	want := "line1\nline2\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_WrappedBuffer_PartialLineOnly(t *testing.T) {
	// Edge case: buffer wraps but the ring portion has no newlines.
	// Nothing to trim to, so return what we have without panicking.
	tw := NewTermWriter(New(20))
	long := bytes.Repeat([]byte("x"), 30)
	tw.Write(long)
	got := tw.Snapshot()
	if len(got) == 0 {
		t.Error("expected non-empty snapshot")
	}
}

// =============================================================
// Interactions between clear and other TermWriter features.
// =============================================================

func TestTermWriter_ClearPreservesPostContent_CRLF(t *testing.T) {
	// Post-clear content with \r\r\n (PTY onlcr) should be handled correctly.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("old\r\r\n"))
	tw.Write([]byte("\x1b[2Jnew line\r\r\n"))
	got := tw.Snapshot()
	want := "new line\r\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_ClearThenSpinner(t *testing.T) {
	// Clear followed by spinner frames. All frames preserved with \r;
	// on replay, \r returns to col 1 and each frame overwrites the previous.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("old junk\n"))
	tw.Write([]byte("\x1b[2J"))
	tw.Write([]byte("⠋ loading\r⠙ loading\r✓ done\n"))
	got := tw.Snapshot()
	want := "⠋ loading\r⠙ loading\r✓ done\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTermWriter_LenAfterClear(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("some data\nmore data\n"))
	if tw.Len() == 0 {
		t.Fatal("expected non-zero len before clear")
	}
	// After clear, only "short\n" remains.
	tw.Write([]byte("\x1b[2Jshort\n"))
	want := len("short\n")
	if tw.Len() != want {
		t.Errorf("Len after clear: got %d, want %d", tw.Len(), want)
	}
}
