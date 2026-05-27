package scrollback

import (
	"bytes"
	"regexp"
	"strings"
)

// escape-sequence markers for pi's synchronized-update protocol.
var (
	bsuMarker   = []byte("\x1b[?2026h") // Begin Synchronized Update
	esuMarker   = []byte("\x1b[?2026l") // End Synchronized Update
	csi3jMarker = []byte("\x1b[3J")     // Erase scrollback
	csiRe       = regexp.MustCompile(`\x1b\[[\d;]*[A-Za-z]`)
)


// ExtractBytes synthesises readable scrollback content from raw PTY bytes.
// Runs server-side (Go) so the client downloads only the compact result
// rather than the full raw file.
//
// Algorithm:
//  1. Scan for BSU…ESU blocks that contain CSI 3J — pi's full-screen redraws
//     (one per conversation turn). All blocks are processed; there is no
//     cap. In Go the O(n×rows) pass over 20k+ blocks takes single-digit ms.
//  2. For adjacent block pairs, find the "anchor" — the deepest line of the
//     previous block still present at the top of the current block — and
//     output only the new lines below it. This stitches consecutive viewport
//     snapshots into a continuous conversation history without duplicates.
//  3. Append any bytes after the last block (stripped of BSU/ESU).
//  4. Fall back to StripSyncBlocks when no full-render block exists.
func ExtractBytes(data []byte) []byte {
	if !bytes.Contains(data, bsuMarker) {
		return StripSyncBlocks(data)
	}

	type blockSpan struct {
		contentStart int
		blockEnd     int
	}

	var blocks []blockSpan
	i := 0
	for i < len(data) {
		bsuRel := bytes.Index(data[i:], bsuMarker)
		if bsuRel < 0 {
			break
		}
		bsuPos := i + bsuRel

		esuRel := bytes.Index(data[bsuPos+len(bsuMarker):], esuMarker)
		if esuRel < 0 {
			break
		}
		esuPos := bsuPos + len(bsuMarker) + esuRel

		// Only retain blocks that contain CSI 3J (full-screen clear).
		inner := data[bsuPos:esuPos]
		if csi3jRel := bytes.Index(inner, csi3jMarker); csi3jRel >= 0 {
			blocks = append(blocks, blockSpan{
				contentStart: bsuPos + csi3jRel + len(csi3jMarker),
				blockEnd:     esuPos + len(esuMarker),
			})
		}
		i = esuPos + len(esuMarker)
	}

	if len(blocks) == 0 {
		return StripSyncBlocks(data)
	}


	getContent := func(b blockSpan) []byte {
		return data[b.contentStart : b.blockEnd-len(esuMarker)]
	}

	// Single block: use it directly + append stripped tail.
	if len(blocks) == 1 {
		content := getContent(blocks[0])
		after := StripSyncBlocks(data[blocks[0].blockEnd:])
		out := make([]byte, len(content)+len(after))
		copy(out, content)
		copy(out[len(content):], after)
		return out
	}

	// Multi-block: stitch blocks via anchor matching.
	blockLines := func(b blockSpan) []string {
		return splitRawLines(getContent(b))
	}

	stripCSI := func(s string) string {
		s = csiRe.ReplaceAllString(s, "")
		s = strings.ReplaceAll(s, "\x00", "")
		return strings.TrimRight(s, " \t")
	}

	// findNewStart returns the index of the first line in currLines
	// that is genuinely new (not carried over from prevLines).
	// Mirrors the TypeScript findNewStart exactly.
	findNewStart := func(prevLines, currLines []string) int {
		const tailSkip = 3  // pi status-bar lines at the bottom of each block
		const verifyLen = 2 // consecutive lines that must match for confidence

		prevConvEnd := len(prevLines) - tailSkip
		if prevConvEnd < 0 {
			prevConvEnd = 0
		}

		prevStripped := make([]string, prevConvEnd)
		for j := 0; j < prevConvEnd; j++ {
			prevStripped[j] = stripCSI(prevLines[j])
		}
		currStripped := make([]string, len(currLines))
		for j, l := range currLines {
			currStripped[j] = stripCSI(l)
		}

		// Build position index of currLines for O(1) lookup.
		currIndex := make(map[string][]int, len(currStripped))
		for c, line := range currStripped {
			if line == "" {
				continue
			}
			currIndex[line] = append(currIndex[line], c)
		}

		// Scan prevConv from the bottom — find deepest carry-over line.
		for p := prevConvEnd - 1; p >= 0; p-- {
			target := prevStripped[p]
			if target == "" {
				continue
			}
			positions := currIndex[target]
			if positions == nil {
				continue
			}
			// Try rightmost occurrence first to maximise the cut point.
			for k := len(positions) - 1; k >= 0; k-- {
				c := positions[k]
				verified := true
				for v := 1; v <= verifyLen; v++ {
					pi, ci := p+v, c+v
					if pi >= prevConvEnd || ci >= len(currStripped) {
						break
					}
					if prevStripped[pi] != currStripped[ci] {
						verified = false
						break
					}
				}
				if verified {
					return c + 1
				}
			}
		}
		return 0
	}

	var parts [][]byte
	parts = append(parts, getContent(blocks[0]))
	prevLines := blockLines(blocks[0])

	for b := 1; b < len(blocks); b++ {
		currLines := blockLines(blocks[b])
		newStart := findNewStart(prevLines, currLines)
		if newStart < len(currLines) {
			newText := strings.Join(currLines[newStart:], "\r\n")
			if strings.TrimSpace(newText) != "" {
				parts = append(parts, []byte("\r\n"+newText))
			}
		}
		prevLines = currLines
	}

	// Append stripped raw bytes after the last block.
	lastBlock := blocks[len(blocks)-1]
	if after := StripSyncBlocks(data[lastBlock.blockEnd:]); len(after) > 0 {
		parts = append(parts, after)
	}

	total := 0
	for _, p := range parts {
		total += len(p)
	}
	out := make([]byte, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// StripSyncBlocks removes all BSU…ESU synchronized-update blocks
// from data, returning only the bytes that fell outside those blocks.
func StripSyncBlocks(data []byte) []byte {
	if !bytes.Contains(data, bsuMarker) {
		return data
	}
	out := make([]byte, 0, len(data))
	i := 0
	for i < len(data) {
		bsuRel := bytes.Index(data[i:], bsuMarker)
		if bsuRel < 0 {
			out = append(out, data[i:]...)
			break
		}
		bsuPos := i + bsuRel
		out = append(out, data[i:bsuPos]...)
		esuRel := bytes.Index(data[bsuPos+len(bsuMarker):], esuMarker)
		if esuRel < 0 {
			break // unterminated block at tail — drop it
		}
		i = bsuPos + len(bsuMarker) + esuRel + len(esuMarker)
	}
	return out
}

// splitRawLines splits a byte slice on \r\n or \n into string lines.
func splitRawLines(data []byte) []string {
	s := string(data)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(s, "\n")
}
