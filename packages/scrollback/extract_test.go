package scrollback

import (
	"strings"
	"testing"
)

const (
	bsu  = "\x1b[?2026h"
	esu  = "\x1b[?2026l"
	c3j  = "\x1b[3J"
	cr   = "\r\n"
)

func block(content string) string {
	return bsu + c3j + content + esu
}

// TestExtractBytesNoBlocks: no BSU/ESU — bytes returned unchanged
// (via StripSyncBlocks which is a no-op when there are no markers).
func TestExtractBytesNoBlocks(t *testing.T) {
	input := []byte("plain text\r\nmore text\r\n")
	got := ExtractBytes(input)
	if string(got) != string(input) {
		t.Errorf("want %q, got %q", input, got)
	}
}

// TestExtractBytesSingleBlock: one full-render block — its content
// is returned directly.
func TestExtractBytesSingleBlock(t *testing.T) {
	content := "line1\r\nline2\r\nstatus bar\r\n"
	input := []byte(block(content))
	got := string(ExtractBytes(input))
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line2") {
		t.Errorf("expected block content in output, got %q", got)
	}
	// Must not contain BSU or ESU markers.
	if strings.Contains(got, bsu) || strings.Contains(got, esu) {
		t.Errorf("output should not contain BSU/ESU markers, got %q", got)
	}
}

// TestExtractBytesTwoBlocks: two consecutive blocks where the second
// adds one new line. The output should contain both blocks' unique
// content without duplicating the carry-over lines.
func TestExtractBytesTwoBlocks(t *testing.T) {
	// Block 1 viewport: lines A, B, C, statusbar
	b1 := block("lineA\r\nlineB\r\nlineC\r\nstatusbar\r\n")
	// Block 2 viewport: lines B, C, D, statusbar  (A scrolled off, D is new)
	b2 := block("lineB\r\nlineC\r\nlineD\r\nstatusbar\r\n")
	got := string(ExtractBytes([]byte(b1 + b2)))

	// lineA must appear (from block 1, now scrolled off)
	if !strings.Contains(got, "lineA") {
		t.Errorf("expected lineA in output; got %q", got)
	}
	// lineD must appear (new in block 2)
	if !strings.Contains(got, "lineD") {
		t.Errorf("expected lineD in output; got %q", got)
	}
	// lineB should not be duplicated
	count := strings.Count(got, "lineB")
	if count > 1 {
		t.Errorf("lineB duplicated (%d times) in output %q", count, got)
	}
}

// TestExtractBytesBlockCapPreservesFirstAndLast: when blocks exceed
// MaxExtractBlocks, block[0] (oldest) is retained and so is the last
// block, ensuring oldest state + recent history are both present.
func TestExtractBytesBlockCapPreservesFirstAndLast(t *testing.T) {
	// Build MaxExtractBlocks + 5 blocks.  Block 0 has unique content
	// "FIRST"; block N has unique content "LAST".
	n := MaxExtractBlocks + 5
	var sb strings.Builder
	for i := 0; i < n; i++ {
		var label string
		switch i {
		case 0:
			label = "FIRST"
		case n - 1:
			label = "LAST"
		default:
			label = "middle"
		}
		sb.WriteString(block("content " + label + "\r\nstatus\r\n"))
	}
	got := string(ExtractBytes([]byte(sb.String())))

	if !strings.Contains(got, "FIRST") {
		t.Errorf("block[0] content missing from capped output: %q", got[:min(200, len(got))])
	}
	if !strings.Contains(got, "LAST") {
		t.Errorf("last block content missing from capped output: %q", got[max(0, len(got)-200):])
	}
}

// TestStripSyncBlocksNoMarkers: passthrough when no BSU present.
func TestStripSyncBlocksNoMarkers(t *testing.T) {
	input := []byte("hello world")
	if got := StripSyncBlocks(input); string(got) != "hello world" {
		t.Errorf("want passthrough, got %q", got)
	}
}

// TestStripSyncBlocksRemovesBlock: bytes inside BSU…ESU are removed;
// bytes before and after are kept.
func TestStripSyncBlocksRemovesBlock(t *testing.T) {
	input := []byte("before" + bsu + "inside" + esu + "after")
	got := string(StripSyncBlocks(input))
	if got != "beforeafter" {
		t.Errorf("want %q, got %q", "beforeafter", got)
	}
}

// TestStripSyncBlocksUnterminatedBlock: BSU without matching ESU is
// dropped at the tail — no panic, no partial output.
func TestStripSyncBlocksUnterminatedBlock(t *testing.T) {
	input := []byte("before" + bsu + "no-esu")
	got := string(StripSyncBlocks(input))
	if got != "before" {
		t.Errorf("want %q, got %q", "before", got)
	}
}

// TestExtractBytesNoCsi3j: BSU/ESU blocks without CSI 3J are treated
// as non-full-render blocks and handled by StripSyncBlocks fallback.
func TestExtractBytesNoCsi3j(t *testing.T) {
	// BSU + ESU without CSI 3J
	input := []byte("text" + bsu + "no-clear" + esu + "more")
	got := string(ExtractBytes(input))
	// The non-full-render block is stripped; surrounding text kept.
	if !strings.Contains(got, "text") || !strings.Contains(got, "more") {
		t.Errorf("surrounding text missing; got %q", got)
	}
	if strings.Contains(got, "no-clear") {
		t.Errorf("block content should be stripped; got %q", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
