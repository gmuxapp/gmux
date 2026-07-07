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
// SCOPE (tracer #1): only user text and assistant text blocks are surfaced.
// Thinking, toolCall, toolResult, and non-message records are skipped — later
// slices add agent_thought_chunk / tool_call to both the stream and this
// snapshot. A missing or unreadable file yields an empty history (nil, nil):
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
		text := extractText(rec.Message.Content)
		if text == "" {
			continue
		}
		out = append(out, Message{Role: role, Content: []ContentBlock{TextBlock(text)}})
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// extractText pulls concatenated text from a pi message content field, which
// is either a plain string or an array of typed blocks. Only "text" blocks
// contribute (thinking/toolCall are ignored this slice).
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}
