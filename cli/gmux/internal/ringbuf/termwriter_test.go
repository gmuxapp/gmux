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
	// Each \r clears currentLine; only the final content survives.
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

func TestTermWriter_ClearScreen_ED2(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("before clear\n"))
	tw.Write([]byte("\x1b[2Jafter clear"))
	got := tw.Snapshot()
	if !bytes.Equal(got, []byte("after clear")) {
		t.Errorf("got %q, want %q", got, "after clear")
	}
}

func TestTermWriter_ClearScrollback_ED3(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("line1\nline2\nline3\n"))
	tw.Write([]byte("\x1b[3Jfresh start"))
	got := tw.Snapshot()
	if !bytes.Equal(got, []byte("fresh start")) {
		t.Errorf("got %q, want %q", got, "fresh start")
	}
}

func TestTermWriter_ClearInMiddleOfChunk(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("junk\x1b[2Jgood stuff\n"))
	got := tw.Snapshot()
	if !bytes.Equal(got, []byte("good stuff\n")) {
		t.Errorf("got %q, want %q", got, "good stuff\n")
	}
}

func TestTermWriter_MultipleClearsKeepsLast(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("a\x1b[2Jb\x1b[2Jc\n"))
	got := tw.Snapshot()
	if !bytes.Equal(got, []byte("c\n")) {
		t.Errorf("got %q, want %q", got, "c\n")
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
