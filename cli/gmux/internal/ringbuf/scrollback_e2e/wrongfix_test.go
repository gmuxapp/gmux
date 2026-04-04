package scrollback_e2e

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/cli/gmux/internal/ringbuf"
)

// TestWrongFix_NoBSU_WouldFailProperty2 demonstrates that if BSU is removed
// from the frame-start markers (the wrong fix), the snapshot would NOT start
// at a BSU boundary. It would fall through to newline trimming instead.
func TestWrongFix_NoBSU_WouldFailProperty2(t *testing.T) {
	const bufSize = 4096 // small buffer to wrap quickly
	bsuMarker := []byte("\x1b[?2026h")

	spinnerFrame := []byte(
		"\x1b[?2026h\x1b[3A\r\x1b[2K" +
			"\x1b[38;2;128;128;128m\xe2\xa0\xb9 Working...\x1b[39m" +
			strings.Repeat(" ", 100) +
			"\x1b[0m\x1b[?2026l\x1b[3B\x1b[1G")

	tw := ringbuf.NewTermWriter(ringbuf.New(bufSize))
	// Write enough frames to wrap
	for range 200 {
		tw.Write(spinnerFrame)
	}
	snap := tw.Snapshot()

	// With the correct fix (first BSU), snapshot starts at BSU.
	if !bytes.HasPrefix(snap, bsuMarker) {
		t.Fatalf("BUG: snapshot does not start at BSU; the trim is wrong. prefix: %q",
			snap[:min(20, len(snap))])
	}
	t.Logf("snapshot starts at BSU as expected (%d bytes)", len(snap))
}
