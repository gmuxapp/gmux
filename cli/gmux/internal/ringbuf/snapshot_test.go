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

func TestSnapshot_WrappedBuffer_TrimsToFirstCursorHome(t *testing.T) {
	// Small buffer that wraps. Data contains cursor-home (ESC[H).
	// After wrapping, trim should find the FIRST ESC[H (skipping the
	// partial leading frame) and preserve everything after it.
	tw := NewTermWriter(New(50))

	tw.Write([]byte("\x1b[Hframe1-line1\r\nframe1-line2\r\n"))
	tw.Write([]byte("\x1b[Hframe2-line1\r\nframe2-line2\r\n"))

	snap := tw.Snapshot()
	if !bytes.Contains(snap, []byte("frame2-line1")) {
		t.Errorf("latest frame missing from snapshot: %q", snap)
	}
	// Snapshot should start with ESC[H (first complete frame boundary).
	if !bytes.HasPrefix(snap, []byte("\x1b[H")) {
		t.Errorf("wrapped snapshot should start at cursor-home, got prefix: %q", snap[:min(10, len(snap))])
	}
}

func TestSnapshot_WrappedBuffer_TrimsToFirstBSU(t *testing.T) {
	// When the buffer wraps, trim to the FIRST BSU to skip the partial
	// leading frame. This preserves all complete frames that fit.
	bsuSeq := "\x1b[?2026h"
	esuSeq := "\x1b[?2026l"

	// Two frames, buffer wraps. The trim skips the partial first frame
	// and starts at the first complete BSU boundary.
	frame1 := bsuSeq + "\x1b[3A" + "\r" + "\x1b[2Kframe1-line1\r\n\x1b[2Kframe1-line2\r\n" + esuSeq
	frame2 := bsuSeq + "\x1b[3A" + "\r" + "\x1b[2Kframe2-line1\r\n\x1b[2Kframe2-line2\r\n" + esuSeq

	tw := NewTermWriter(New(len(frame1) + 10))
	tw.Write([]byte(frame1))
	tw.Write([]byte(frame2))

	snap := tw.Snapshot()
	if !bytes.Contains(snap, []byte("frame2-line1")) {
		t.Errorf("latest frame missing: %q", snap)
	}
	// Should start with BSU (the first complete frame after the wrap).
	if !bytes.HasPrefix(snap, []byte(bsuSeq)) {
		t.Errorf("should start at first BSU, got prefix: %q", snap[:min(12, len(snap))])
	}
}

func TestSnapshot_WrappedBuffer_TrimsToFirstCursorHome11(t *testing.T) {
	// Same test but with ESC[1;1H instead of ESC[H.
	tw := NewTermWriter(New(50))

	tw.Write([]byte("\x1b[1;1Hframe1-aaaa\r\nframe1-bbbb\r\n"))
	tw.Write([]byte("\x1b[1;1Hframe2-aaaa\r\nframe2-bbbb\r\n"))

	snap := tw.Snapshot()
	if !bytes.Contains(snap, []byte("frame2-aaaa")) {
		t.Errorf("latest frame missing: %q", snap)
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

func TestSnapshot_WrappedBuffer_FrameStartSkipsPartialLeading(t *testing.T) {
	// When the buffer wraps, the snapshot starts mid-frame. Trimming to
	// the FIRST frame-start marker skips just the partial leading frame,
	// preserving all subsequent complete frames.

	t.Run("cursor-home frames", func(t *testing.T) {
		// Each frame is 24 bytes; 5 frames = 120 bytes. Buffer of 80 wraps.
		// After wrap, the first few bytes are mid-frame garbage. Trim to
		// the first ESC[H skips just that partial frame.
		tw := NewTermWriter(New(80))
		for i := 0; i < 5; i++ {
			tw.Write([]byte("\x1b[Hline1\r\nline2\r\nline3\r\n"))
		}
		snap := tw.Snapshot()
		// Should start at a cursor-home (first clean frame).
		if !bytes.HasPrefix(snap, []byte("\x1b[H")) {
			t.Errorf("should start at cursor-home, got prefix: %q", snap[:min(10, len(snap))])
		}
		// Multiple complete frames should be present (not just the last one).
		count := bytes.Count(snap, []byte("line1"))
		if count < 2 {
			t.Errorf("expected multiple frames preserved, got %d in: %q", count, snap)
		}
	})

	t.Run("BSU frames", func(t *testing.T) {
		bsuSeq := "\x1b[?2026h"
		makeFrame := func(content string) []byte {
			return []byte(bsuSeq + "\x1b[3A\r" + content + "\r\n\x1b[?2026l")
		}
		// Buffer smaller than 3 frames.
		tw := NewTermWriter(New(100))
		for i := 0; i < 5; i++ {
			tw.Write(makeFrame("unique-content"))
		}
		snap := tw.Snapshot()
		// Should start at a BSU (first clean frame).
		if !bytes.HasPrefix(snap, []byte(bsuSeq)) {
			t.Errorf("should start at BSU, got prefix: %q", snap[:min(12, len(snap))])
		}
		// Latest content must be present.
		if !bytes.Contains(snap, []byte("unique-content")) {
			t.Errorf("expected 'unique-content' in snapshot: %q", snap)
		}
	})
}

// =============================================================
// firstFrameStart unit tests.
// =============================================================

func TestFirstFrameStart_EscH(t *testing.T) {
	data := []byte("hello\x1b[Hworld")
	idx := firstFrameStart(data)
	if idx != 5 {
		t.Errorf("expected 5, got %d", idx)
	}
}

func TestFirstFrameStart_Esc11H(t *testing.T) {
	data := []byte("hello\x1b[1;1Hworld")
	idx := firstFrameStart(data)
	if idx != 5 {
		t.Errorf("expected 5, got %d", idx)
	}
}

func TestFirstFrameStart_BSU(t *testing.T) {
	data := []byte("hello\x1b[?2026hworld")
	idx := firstFrameStart(data)
	if idx != 5 {
		t.Errorf("expected 5, got %d", idx)
	}
}

func TestFirstFrameStart_ReturnsFirst(t *testing.T) {
	// Multiple markers: must return the FIRST one.
	data := []byte("\x1b[Hfirst\x1b[?2026hsecond\x1b[Hthird")
	idx := firstFrameStart(data)
	if idx != 0 {
		t.Errorf("expected 0 (first marker), got %d", idx)
	}
}

func TestFirstFrameStart_BSUBeforeCursorHome(t *testing.T) {
	data := []byte("junk\x1b[?2026hframe\x1b[Hhome")
	idx := firstFrameStart(data)
	// BSU at offset 4 comes first.
	if idx != 4 {
		t.Errorf("expected 4 (BSU), got %d", idx)
	}
}

func TestFirstFrameStart_None(t *testing.T) {
	data := []byte("no markers here\x1b[32mcolor\x1b[0m")
	idx := firstFrameStart(data)
	if idx != -1 {
		t.Errorf("expected -1, got %d", idx)
	}
}

func TestFirstFrameStart_NotAmbiguous(t *testing.T) {
	// ESC[3H is "cursor to row 3", NOT cursor home. Must not match.
	data := []byte("text\x1b[3Hmore")
	idx := firstFrameStart(data)
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
	// Pi-tui pattern with a small buffer that wraps. Trim to first BSU
	// skips only the partial leading frame, preserving all complete frames.
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

	// The latest frame's content must be present.
	if !bytes.Contains(snap, []byte("frame3-response")) {
		t.Errorf("latest frame missing: %q", snap)
	}
	// Should start at a BSU (first complete frame).
	if !bytes.HasPrefix(snap, []byte("\x1b[?2026h")) {
		t.Errorf("should start at BSU, got prefix: %q", snap[:min(12, len(snap))])
	}
}
