package ringbuf

import (
	"bytes"
	"testing"
)

// =============================================================
// Bare CR preserves cursor positioning for TUI renderers.
// =============================================================

func TestSnapshot_BareCR_PreservesCursorPositioning(t *testing.T) {
	tw := NewTermWriter(New(4096))
	tw.Write([]byte("\x1b[2J\x1b[H"))

	// Renderer writes content at (5,1), then uses \r + relative move
	// (ultraviolet method #2) to reach (7,1).
	tw.Write([]byte(
		"\x1b[5;1Hcontent at row 5\x1b[K" +
			"\r" +
			"\x1b[7;1Hcontent at row 7\x1b[K",
	))

	snap := tw.Snapshot()
	if !bytes.Contains(snap, []byte("content at row 5")) {
		t.Errorf("cursor-positioned content at row 5 lost to bare CR: %q", snap)
	}
	if !bytes.Contains(snap, []byte("content at row 7")) {
		t.Errorf("missing content at row 7: %q", snap)
	}
}

func TestSnapshot_BareCR_PreservesFrameTransition(t *testing.T) {
	tw := NewTermWriter(New(4096))
	tw.Write([]byte("\x1b[2J\x1b[H"))

	// Frame 1
	tw.Write([]byte(
		"\x1b[?25l\x1b[1;1H" +
			"Header\x1b[K\r\n" +
			"Content\x1b[K\r\n" +
			"Status\x1b[K" +
			"\x1b[?25h",
	))

	// Frame 2: cursor-up + \r (method #2 positioning)
	tw.Write([]byte(
		"\x1b[?25l\x1b[3A" +
			"\r" +
			"New header\x1b[K\r\n" +
			"New content\x1b[K\r\n" +
			"New status\x1b[K" +
			"\x1b[?25h",
	))

	snap := tw.Snapshot()

	if !bytes.Contains(snap, []byte("New header")) {
		t.Errorf("missing frame 2: %q", snap)
	}
	// Cursor-up must be preserved for correct replay positioning
	if !bytes.Contains(snap, []byte("\x1b[3A")) {
		t.Errorf("cursor-up lost to bare CR (frames will stack): %q", snap)
	}
}

// =============================================================
// Spinner frames: preserved with \r, visually collapsed on replay.
// =============================================================

func TestSnapshot_SpinnerNotCollapsed(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("⠋ thinking...\r⠙ thinking...\r⠹ thinking..."))

	snap := tw.Snapshot()
	// All spinner frames preserved with \r. On replay, each \r returns
	// to col 1 and the next frame overwrites visually.
	if !bytes.Contains(snap, []byte("⠹ thinking...")) {
		t.Errorf("missing latest spinner frame: %q", snap)
	}
	// Earlier frames are also present (not collapsed)
	if !bytes.Contains(snap, []byte("⠋ thinking...")) {
		t.Errorf("spinner frames should be preserved with \\r: %q", snap)
	}
}

func TestSnapshot_SpinnerWithPendingCR(t *testing.T) {
	// Spinner frame ends with \r deferred to next chunk.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("spinner frame\r"))

	snap := tw.Snapshot()
	// Should include the pending \r for correct rendering.
	if !bytes.HasSuffix(snap, []byte("spinner frame\r")) {
		t.Errorf("snapshot should include pending CR: %q", snap)
	}

	// Next PTY data resolves the CR (bare CR: flushed with \r).
	// On replay: "spinner frame" then \r (col 1), then "final frame\n"
	tw.Write([]byte("final frame\n"))
	snap = tw.Snapshot()
	want := "spinner frame\rfinal frame\n"
	if !bytes.Equal(snap, []byte(want)) {
		t.Errorf("got %q, want %q", snap, want)
	}
}

// =============================================================
// Wrapped buffer: smart trim to last cursor-home.
// =============================================================

func TestSnapshot_WrappedBuffer_TrimsToLastCursorHome(t *testing.T) {
	// Small buffer that wraps. Data contains cursor-home (ESC[H).
	// After wrapping, trim should find the last ESC[H and start there.
	// Each frame is ~31 bytes; buffer of 50 wraps on second frame.
	tw := NewTermWriter(New(50))

	tw.Write([]byte("\x1b[Hframe1-line1\r\nframe1-line2\r\n"))
	tw.Write([]byte("\x1b[Hframe2-line1\r\nframe2-line2\r\n"))

	snap := tw.Snapshot()
	// Should have trimmed to the last ESC[H (start of frame 2).
	if !bytes.Contains(snap, []byte("frame2-line1")) {
		t.Errorf("latest frame missing from snapshot: %q", snap)
	}
	// Frame 1 content should be gone (trimmed away on wrap).
	if bytes.Contains(snap, []byte("frame1-line1")) {
		t.Errorf("stale frame should be trimmed from wrapped snapshot: %q", snap)
	}
	// Snapshot should start with ESC[H
	if !bytes.HasPrefix(snap, []byte("\x1b[H")) {
		t.Errorf("wrapped snapshot should start at cursor-home, got prefix: %q", snap[:min(10, len(snap))])
	}
}

func TestSnapshot_WrappedBuffer_TrimsToLastBSU(t *testing.T) {
	// pi-tui wraps every differential render frame in BSU/ESU.
	// When the buffer wraps, we should trim to the last BSU.
	bsuSeq := "\x1b[?2026h"
	esuSeq := "\x1b[?2026l"

	// Each frame: BSU + cursor-up + \r + content + ESU ≈ 50 bytes
	frame1 := bsuSeq + "\x1b[3A" + "\r" + "\x1b[2Kframe1-line1\r\n\x1b[2Kframe1-line2\r\n" + esuSeq
	frame2 := bsuSeq + "\x1b[3A" + "\r" + "\x1b[2Kframe2-line1\r\n\x1b[2Kframe2-line2\r\n" + esuSeq

	// Buffer smaller than both frames combined
	tw := NewTermWriter(New(len(frame1) + 10))
	tw.Write([]byte(frame1))
	tw.Write([]byte(frame2))

	snap := tw.Snapshot()
	if !bytes.Contains(snap, []byte("frame2-line1")) {
		t.Errorf("latest frame missing: %q", snap)
	}
	if bytes.Contains(snap, []byte("frame1-line1")) {
		t.Errorf("stale frame should be trimmed: %q", snap)
	}
	// Should start with BSU
	if !bytes.HasPrefix(snap, []byte(bsuSeq)) {
		t.Errorf("wrapped snapshot should start at BSU, got prefix: %q", snap[:min(12, len(snap))])
	}
}

func TestSnapshot_WrappedBuffer_TrimsToLastCursorHome11(t *testing.T) {
	// Same test but with ESC[1;1H instead of ESC[H.
	// Each frame is ~32 bytes; buffer of 50 wraps on second frame.
	tw := NewTermWriter(New(50))

	tw.Write([]byte("\x1b[1;1Hframe1-aaaa\r\nframe1-bbbb\r\n"))
	tw.Write([]byte("\x1b[1;1Hframe2-aaaa\r\nframe2-bbbb\r\n"))

	snap := tw.Snapshot()
	if !bytes.Contains(snap, []byte("frame2-aaaa")) {
		t.Errorf("latest frame missing: %q", snap)
	}
	if bytes.Contains(snap, []byte("frame1-aaaa")) {
		t.Errorf("stale frame should be trimmed: %q", snap)
	}
	if !bytes.HasPrefix(snap, []byte("\x1b[1;1H")) {
		t.Errorf("should start at ESC[1;1H, got: %q", snap[:min(10, len(snap))])
	}
}

func TestSnapshot_WrappedBuffer_NoCursorHome_FallsBackToNewline(t *testing.T) {
	// Wrapped buffer with no cursor-home: falls back to first newline trim.
	tw := NewTermWriter(New(40))
	tw.Write([]byte("aaaaaaaaaa\n")) // 11 bytes
	tw.Write([]byte("bbbbbbbbbb\n")) // 11 bytes (22)
	tw.Write([]byte("cccccccccc\n")) // 11 bytes (33)
	tw.Write([]byte("dddddddddd\n")) // 11 bytes (44, wrapped)

	snap := tw.Snapshot()
	// No cursor-home in data, falls back to first newline trim.
	firstNewline := bytes.IndexByte(snap, '\n')
	if firstNewline < 0 {
		t.Fatalf("no newline in snapshot: %q", snap)
	}
	firstLine := snap[:firstNewline]
	if len(firstLine) != 10 {
		t.Errorf("first line should be complete 10-char line, got %q (len %d)", firstLine, len(firstLine))
	}
}

func TestSnapshot_WrappedBuffer_FrameStartPreventsDuplication(t *testing.T) {
	// Core bug scenario: multiple TUI frames accumulate, and replaying
	// them all on a smaller terminal causes old frames to scroll into
	// xterm.js scrollback, creating visible "duplicate groups."
	//
	// Fix: trim to the last frame-start marker (BSU or cursor-home)
	// so only the latest frame is replayed.

	t.Run("cursor-home frames", func(t *testing.T) {
		// Each frame is 24 bytes; 5 frames = 120 bytes. Buffer of 80 wraps.
		tw := NewTermWriter(New(80))
		for i := 0; i < 5; i++ {
			tw.Write([]byte("\x1b[Hline1\r\nline2\r\nline3\r\n"))
		}
		snap := tw.Snapshot()
		count := bytes.Count(snap, []byte("line1"))
		if count != 1 {
			t.Errorf("expected 1 occurrence of 'line1', got %d in: %q", count, snap)
		}
	})

	t.Run("BSU frames (pi-tui pattern)", func(t *testing.T) {
		bsuSeq := "\x1b[?2026h"
		// Simulates pi's differential render: BSU + cursor-up + CR + content + ESU
		makeFrame := func(content string) []byte {
			return []byte(bsuSeq + "\x1b[3A\r" + content + "\r\n\x1b[?2026l")
		}
		// Buffer smaller than 3 frames
		tw := NewTermWriter(New(100))
		for i := 0; i < 5; i++ {
			tw.Write(makeFrame("unique-content"))
		}
		snap := tw.Snapshot()
		// Should have at most 1 occurrence (latest frame)
		count := bytes.Count(snap, []byte("unique-content"))
		if count != 1 {
			t.Errorf("expected 1 occurrence of 'unique-content', got %d in: %q", count, snap)
		}
	})
}

// =============================================================
// lastFrameStart unit tests.
// =============================================================

func TestLastFrameStart_EscH(t *testing.T) {
	data := []byte("hello\x1b[Hworld")
	idx := lastFrameStart(data)
	if idx != 5 {
		t.Errorf("expected 5, got %d", idx)
	}
}

func TestLastFrameStart_Esc11H(t *testing.T) {
	data := []byte("hello\x1b[1;1Hworld")
	idx := lastFrameStart(data)
	if idx != 5 {
		t.Errorf("expected 5, got %d", idx)
	}
}

func TestLastFrameStart_BSU(t *testing.T) {
	data := []byte("hello\x1b[?2026hworld")
	idx := lastFrameStart(data)
	if idx != 5 {
		t.Errorf("expected 5, got %d", idx)
	}
}

func TestLastFrameStart_BSU_BeatsOlderCursorHome(t *testing.T) {
	// BSU after cursor-home: BSU wins because it's later.
	data := []byte("\x1b[Hfirst\x1b[?2026hsecond")
	idx := lastFrameStart(data)
	// BSU is at offset 8 (after ESC[H + "first")
	want := bytes.Index(data, []byte("\x1b[?2026h"))
	if idx != want {
		t.Errorf("expected %d (BSU position), got %d", want, idx)
	}
}

func TestLastFrameStart_Multiple(t *testing.T) {
	data := []byte("\x1b[Hfirst\x1b[Hsecond\x1b[Hthird")
	idx := lastFrameStart(data)
	// Should find the LAST one (before "third")
	expected := bytes.LastIndex(data, []byte("\x1b[H"))
	if idx != expected {
		t.Errorf("expected %d, got %d", expected, idx)
	}
}

func TestLastFrameStart_None(t *testing.T) {
	data := []byte("no markers here\x1b[32mcolor\x1b[0m")
	idx := lastFrameStart(data)
	if idx != -1 {
		t.Errorf("expected -1, got %d", idx)
	}
}

func TestLastFrameStart_NotAmbiguous(t *testing.T) {
	// ESC[3H is "cursor to row 3", NOT cursor home. Must not match.
	data := []byte("text\x1b[3Hmore")
	idx := lastFrameStart(data)
	if idx != -1 {
		t.Errorf("ESC[3H should not match cursor home, got idx=%d", idx)
	}
}

// =============================================================
// Edge cases.
// =============================================================

func TestSnapshot_MultipleWritesPerLine(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("hello "))
	tw.Write([]byte("world"))
	tw.Write([]byte("\n"))
	got := tw.Snapshot()
	want := "hello world\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSnapshot_PendingCR_CRLF(t *testing.T) {
	tw := NewTermWriter(New(256))
	tw.Write([]byte("hello\r"))
	tw.Write([]byte("\n"))
	got := tw.Snapshot()
	want := "hello\r\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSnapshot_PendingCR_BareCR_MultipleCRs_ThenText(t *testing.T) {
	// Three CRs followed by a printable char: bare CR, flushed with \r.
	tw := NewTermWriter(New(256))
	tw.Write([]byte("old\r"))
	tw.Write([]byte("\r\rnew line\n"))
	got := tw.Snapshot()
	want := "old\rnew line\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// =============================================================
// Pi-tui realistic rendering patterns.
// =============================================================

func TestSnapshot_PiTui_DifferentialRender(t *testing.T) {
	// Simulates pi-tui's actual differential rendering: each frame is
	// BSU + cursor-up + CR + clear-line + content lines + ESU.
	// On a non-wrapped buffer, both frames are present and the cursor
	// positioning produces the correct visual result on replay.
	tw := NewTermWriter(New(4096))

	// Frame 1 (initial render)
	tw.Write([]byte(
		"\x1b[?2026h" +
			"\x1b[2K" + "version v0.62.0" + "\r\n" +
			"\x1b[2K" + "> say hello" + "\r\n" +
			"\x1b[2K" + "thinking..." +
			"\x1b[?2026l",
	))

	// Frame 2 (response arrives, differential redraw)
	tw.Write([]byte(
		"\x1b[?2026h" +
			"\x1b[3A" + "\r" +
			"\x1b[2K" + "version v0.62.0" + "\r\n" +
			"\x1b[2K" + "> say hello" + "\r\n" +
			"\x1b[2K" + "Hello! How can I help?" +
			"\x1b[?2026l",
	))

	snap := tw.Snapshot()

	// Both frames should be present (buffer hasn't wrapped)
	if !bytes.Contains(snap, []byte("thinking...")) {
		t.Errorf("frame 1 content missing: %q", snap)
	}
	if !bytes.Contains(snap, []byte("Hello! How can I help?")) {
		t.Errorf("frame 2 content missing: %q", snap)
	}
	// Cursor-up must be preserved for frame 2 to overwrite frame 1
	if !bytes.Contains(snap, []byte("\x1b[3A")) {
		t.Errorf("cursor-up lost (frames will stack on replay): %q", snap)
	}
}

func TestSnapshot_PiTui_WrappedDifferentialRender(t *testing.T) {
	// Same pi-tui pattern but with a small buffer that wraps.
	// Only the latest frame should survive after trim.
	makeFrame := func(line3 string) []byte {
		return []byte(
			"\x1b[?2026h" +
				"\x1b[3A" + "\r" +
				"\x1b[2K" + "header" + "\r\n" +
				"\x1b[2K" + "prompt" + "\r\n" +
				"\x1b[2K" + line3 +
				"\x1b[?2026l",
		)
	}

	// Each frame is roughly 60 bytes. Buffer of 80 wraps after 2 frames.
	tw := NewTermWriter(New(80))
	tw.Write(makeFrame("frame1-response"))
	tw.Write(makeFrame("frame2-response"))
	tw.Write(makeFrame("frame3-response"))

	snap := tw.Snapshot()

	// Only the latest frame should be present
	if !bytes.Contains(snap, []byte("frame3-response")) {
		t.Errorf("latest frame missing: %q", snap)
	}
	if bytes.Contains(snap, []byte("frame1-response")) {
		t.Errorf("stale frame1 should be trimmed: %q", snap)
	}
}
