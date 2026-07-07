package acp

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// LoadHistory reads a pi JSONL conversation file and returns the user +
// assistant text messages as the ACP history snapshot.
//
// SCOPE (slice #2): user text plus assistant text AND thinking blocks are
// surfaced, preserving their in-message order. toolCall / toolResult and
// non-message records are skipped — a later slice adds tool_call to both the
// stream and this snapshot. A missing or unreadable file yields an empty
// history (nil, nil):
// a brand-new session whose JSONL isn't written yet is not an error.
func LoadHistory(path string) ([]Message, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Message
	sc := bufio.NewScanner(f)
	// pi messages can be large (long assistant turns); raise the line cap.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			Message *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // tolerate partial/corrupt trailing lines
		}
		if rec.Type != "message" || rec.Message == nil {
			continue
		}
		role := rec.Message.Role
		if role != "user" && role != "assistant" {
			continue // drop toolResult and anything else this slice
		}
		blocks := extractBlocks(rec.Message.Content)
		if len(blocks) == 0 {
			continue
		}
		out = append(out, Message{Role: role, Content: blocks})
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// extractBlocks pulls the renderable content blocks from a pi message content
// field, which is either a plain string or an array of typed blocks. "text"
// and "thinking" blocks are surfaced in order (pi stores thinking text under
// `.thinking`, not `.text`); toolCall/toolResult blocks are skipped this slice.
func extractBlocks(raw json.RawMessage) []ContentBlock {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []ContentBlock{TextBlock(s)}
	}
	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	var out []ContentBlock
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				out = append(out, TextBlock(blk.Text))
			}
		case "thinking":
			if blk.Thinking != "" {
				out = append(out, ThinkingBlock(blk.Thinking))
			}
		}
	}
	return out
}
