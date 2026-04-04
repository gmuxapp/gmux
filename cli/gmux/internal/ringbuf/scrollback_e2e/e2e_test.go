// Package scrollback_e2e contains end-to-end tests that verify the
// TermWriter scrollback buffer against real recorded pi sessions.
// See README.md in this directory for background and instructions.
package scrollback_e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/cli/gmux/internal/ringbuf"
	"github.com/vito/midterm"
)

// renderScreen feeds data through a midterm VT100 emulator (rows x cols)
// and returns all rows with trailing whitespace trimmed. The returned
// slice always has exactly `rows` entries so that row positions are
// preserved in comparisons.
func renderScreen(data []byte, rows, cols int) []string {
	vt := midterm.NewTerminal(rows, cols)
	vt.Write(data)

	lines := make([]string, rows)
	for row := range rows {
		lines[row] = strings.TrimRight(string(vt.Content[row]), " ")
	}
	return lines
}

// feedWhole writes all data to tw in a single Write call.
func feedWhole(tw *ringbuf.TermWriter, data []byte) {
	tw.Write(data)
}

// feedChunked writes data to tw in fixed-size chunks (997 bytes, a prime
// number to avoid alignment with any protocol framing). This exercises
// the cross-chunk escBuf and pendingCR mechanisms with real data.
func feedChunked(tw *ringbuf.TermWriter, data []byte) {
	const chunk = 997
	for i := 0; i < len(data); i += chunk {
		end := i + chunk
		if end > len(data) {
			end = len(data)
		}
		tw.Write(data[i:end])
	}
}

// loadFixtures returns all .bin files in testdata/.
func loadFixtures(t *testing.T) []struct {
	name string
	data []byte
} {
	t.Helper()
	matches, err := filepath.Glob("testdata/*.bin")
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}
	if len(matches) == 0 {
		t.Skip("no fixtures in testdata/")
	}

	var fixtures []struct {
		name string
		data []byte
	}
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		name := strings.TrimSuffix(filepath.Base(path), ".bin")
		fixtures = append(fixtures, struct {
			name string
			data []byte
		}{name, data})
	}
	return fixtures
}

// assertScreensMatch renders raw and snap through a VT100 emulator and
// asserts that every row is identical.
func assertScreensMatch(t *testing.T, raw, snap []byte, rows, cols int) {
	t.Helper()

	rawScreen := renderScreen(raw, rows, cols)
	snapScreen := renderScreen(snap, rows, cols)

	// Log non-empty lines for debugging.
	t.Log("--- raw screen ---")
	for row, line := range rawScreen {
		if line != "" {
			t.Logf("  row %2d: %s", row, line)
		}
	}
	t.Log("--- scrollback screen ---")
	for row, line := range snapScreen {
		if line != "" {
			t.Logf("  row %2d: %s", row, line)
		}
	}

	// Compare every row (including empty ones) so row position
	// differences are caught.
	diffs := 0
	for row := range rows {
		if rawScreen[row] != snapScreen[row] {
			diffs++
			t.Errorf("row %d differs:\n  raw:  %q\n  snap: %q",
				row, rawScreen[row], snapScreen[row])
		}
	}
	if diffs == 0 {
		t.Log("scrollback replay matches original screen")
	}
}

// TestScrollbackMatchesScreen verifies that the TermWriter's scrollback
// buffer, when replayed through a real terminal emulator, produces the
// same visible screen as the original raw PTY output.
//
// Each fixture is a recorded pi session (testdata/*.bin) captured from a
// real pi TUI interaction through a PTY. See README.md for how to record
// new fixtures.
//
// Both the raw recording and the TermWriter scrollback snapshot are fed
// through a VT100 emulator (vito/midterm). The resulting visible screens
// must match: what the user saw originally is what a reconnecting client
// would see when replaying the scrollback.
//
// Two feed modes are tested for each fixture:
//   - single_write: the entire recording in one Write call
//   - chunked_writes: the recording split into 997-byte chunks (exercises
//     cross-chunk escape sequence and CR handling)
func TestScrollbackMatchesScreen(t *testing.T) {
	const (
		rows = 40
		cols = 120
	)

	feedModes := []struct {
		name string
		feed func(*ringbuf.TermWriter, []byte)
	}{
		{"single_write", feedWhole},
		{"chunked_writes", feedChunked},
	}

	for _, fix := range loadFixtures(t) {
		t.Run(fix.name, func(t *testing.T) {
			t.Logf("fixture: %d bytes", len(fix.data))

			for _, mode := range feedModes {
				t.Run(mode.name, func(t *testing.T) {
					tw := ringbuf.NewTermWriter(ringbuf.New(128 * 1024))
					mode.feed(tw, fix.data)
					snap := tw.Snapshot()
					t.Logf("scrollback snapshot: %d bytes (%.0f%% of raw)",
						len(snap), float64(len(snap))/float64(len(fix.data))*100)

					assertScreensMatch(t, fix.data, snap, rows, cols)
				})
			}
		})
	}
}

// TestScrollbackChunkedMatchesSingleWrite verifies that chunked writes
// produce the exact same scrollback snapshot as a single write. This
// catches bugs in the cross-chunk escape sequence reassembly (escBuf)
// and deferred CR handling (pendingCR).
func TestScrollbackChunkedMatchesSingleWrite(t *testing.T) {
	for _, fix := range loadFixtures(t) {
		t.Run(fix.name, func(t *testing.T) {
			twWhole := ringbuf.NewTermWriter(ringbuf.New(128 * 1024))
			feedWhole(twWhole, fix.data)
			snapWhole := twWhole.Snapshot()

			twChunked := ringbuf.NewTermWriter(ringbuf.New(128 * 1024))
			feedChunked(twChunked, fix.data)
			snapChunked := twChunked.Snapshot()

			if !bytes.Equal(snapWhole, snapChunked) {
				t.Errorf("snapshots differ: single_write=%d bytes, chunked=%d bytes",
					len(snapWhole), len(snapChunked))
				// Find first divergence point for debugging.
				minLen := len(snapWhole)
				if len(snapChunked) < minLen {
					minLen = len(snapChunked)
				}
				for i := 0; i < minLen; i++ {
					if snapWhole[i] != snapChunked[i] {
						ctx := 40
						start := i - ctx
						if start < 0 {
							start = 0
						}
						endW := i + ctx
						if endW > len(snapWhole) {
							endW = len(snapWhole)
						}
						endC := i + ctx
						if endC > len(snapChunked) {
							endC = len(snapChunked)
						}
						t.Errorf("first difference at byte %d:\n  single:  %q\n  chunked: %q",
							i, snapWhole[start:endW], snapChunked[start:endC])
						break
					}
				}
			}
		})
	}
}

// TestScrollbackSmallerThanRaw is a sanity check: for any recording that
// contains a screen clear, the snapshot must be strictly smaller than the
// raw input. The clear discards pre-clear content; if the snapshot is the
// same size or larger, something is wrong.
func TestScrollbackSmallerThanRaw(t *testing.T) {
	for _, fix := range loadFixtures(t) {
		t.Run(fix.name, func(t *testing.T) {
			// Verify the fixture actually contains a screen clear.
			if !bytes.Contains(fix.data, []byte("\x1b[2J")) {
				t.Skip("fixture has no screen clear")
			}

			tw := ringbuf.NewTermWriter(ringbuf.New(128 * 1024))
			tw.Write(fix.data)
			snap := tw.Snapshot()

			if len(snap) >= len(fix.data) {
				t.Errorf("snapshot (%d bytes) should be smaller than raw (%d bytes)",
					len(snap), len(fix.data))
			}
			t.Logf("raw=%d snap=%d (%.0f%% reduction)",
				len(fix.data), len(snap),
				(1-float64(len(snap))/float64(len(fix.data)))*100)
		})
	}
}

// TestScrollbackWrappedBSU simulates what happens after a pi recording
// ends: the TUI continues emitting BSU/ESU differential render frames at
// ~13 Hz (spinner, status bar updates). These small frames accumulate
// until the 128KB ring buffer wraps.
//
// Two properties are verified:
//
//  1. The snapshot retains close to the full buffer capacity (~128KB),
//     not just one BSU frame (~159 bytes). This catches the original bug
//     where trimWrappedSnapshot trimmed to the LAST BSU.
//
//  2. The snapshot starts at a BSU marker (a clean frame boundary), not
//     at an arbitrary byte mid-frame. This verifies the trim finds the
//     FIRST frame start, skipping only the partial leading frame.
//
// The spinner frame below is extracted verbatim from pi_thinking_session.bin.
// It contains \r but no \n (real pi idle renders are CR-only). Without BSU
// matching, the trim has no newlines to fall back to (strategy 2 fails) and
// returns the raw wrapped buffer at an arbitrary byte (strategy 3).
func TestScrollbackWrappedBSU(t *testing.T) {
	const bufSize = 128 * 1024
	bsuMarker := []byte("\x1b[?2026h")

	// Real pi spinner frame extracted from pi_thinking_session.bin.
	// BSU + CR + erase-line + content + padding + reset + OSC + ESU.
	// 159 bytes, contains \r, no \n.
	spinnerFrame := []byte(
		"\x1b[?2026h" + // BSU
			"\r\x1b[2K" + // CR + erase line
			"W\x1b[7m \x1b[0m" + // "W" + reversed space + reset
			strings.Repeat(" ", 118) + // padding to terminal width
			"\x1b[0m\x1b]8;;\x07" + // reset + empty OSC hyperlink
			"\x1b[?2026l", // ESU
	)

	for _, fix := range loadFixtures(t) {
		t.Run(fix.name, func(t *testing.T) {
			if !bytes.Contains(fix.data, []byte("\x1b[2J")) {
				t.Skip("fixture has no screen clear")
			}

			// Feed the real recording.
			tw := ringbuf.NewTermWriter(ringbuf.New(bufSize))
			tw.Write(fix.data)
			t.Logf("after fixture: len=%d bytes", tw.Len())

			// Simulate ~10 minutes of idle rendering.
			nFrames := (bufSize * 3) / len(spinnerFrame)
			for range nFrames {
				tw.Write(spinnerFrame)
			}
			snap := tw.Snapshot()
			t.Logf("after %d spinner frames: snapshot=%d bytes", nFrames, len(snap))

			// Property 1: snapshot retains the full buffer, not one frame.
			minExpected := bufSize / 2
			if len(snap) < minExpected {
				t.Errorf("snapshot too small: %d bytes (expected >=%d); "+
					"looks like trim-to-last-BSU bug",
					len(snap), minExpected)
			}

			// Property 2: snapshot starts at a clean BSU frame boundary.
			// The spinner frames are CR-only (no newlines), so if BSU
			// matching were broken the trim would fall through to
			// strategy 3 (no trim) and start at an arbitrary byte.
			if !bytes.HasPrefix(snap, bsuMarker) {
				prefix := snap[:min(20, len(snap))]
				t.Errorf("snapshot should start at BSU frame boundary, "+
					"got prefix: %q", prefix)
			}
		})
	}
}
