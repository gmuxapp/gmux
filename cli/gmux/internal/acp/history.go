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
// SCOPE (slice #3): user text plus assistant text, thinking, AND tool-call
// blocks are surfaced, preserving their in-message order. A separate toolResult
// record is correlated back to its originating toolCall block by id, filling in
// the block's status and output. Non-message records are skipped. A missing or
// unreadable file yields an empty history (nil, nil): a brand-new session whose
// JSONL isn't written yet is not an error.
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
	// Index every tool-call block by its id so a later toolResult record can be
	// correlated back to it (blocks live in out[*].Content; track positions
	// rather than pointers, which slice growth would invalidate).
	type blockRef struct{ msg, blk int }
	toolCalls := map[string]blockRef{}

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
				Role       string          `json:"role"`
				Content    json.RawMessage `json:"content"`
				ToolCallID string          `json:"toolCallId"`
				IsError    bool            `json:"isError"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // tolerate partial/corrupt trailing lines
		}
		if rec.Type != "message" || rec.Message == nil {
			continue
		}
		role := rec.Message.Role
		if role == "toolResult" {
			// Correlate the result back to its tool-call block by id, filling in
			// the terminal status and textual output.
			ref, ok := toolCalls[rec.Message.ToolCallID]
			if !ok {
				continue
			}
			blk := &out[ref.msg].Content[ref.blk]
			blk.Output = extractText(rec.Message.Content)
			if rec.Message.IsError {
				blk.Status = ToolStatusFailed
			} else {
				blk.Status = ToolStatusCompleted
			}
			continue
		}
		if role != "user" && role != "assistant" {
			continue // drop anything else
		}
		blocks := extractBlocks(rec.Message.Content)
		if len(blocks) == 0 {
			continue
		}
		msgIdx := len(out)
		out = append(out, Message{Role: role, Content: blocks})
		for i := range blocks {
			if blocks[i].Type == ContentTypeToolCall && blocks[i].ToolCallID != "" {
				toolCalls[blocks[i].ToolCallID] = blockRef{msg: msgIdx, blk: i}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// extractBlocks pulls the renderable content blocks from a pi message content
// field, which is either a plain string or an array of typed blocks. "text"
// and "thinking" blocks are surfaced in order (pi stores thinking text under
// `.thinking`, not `.text`); "toolCall" blocks become in-progress tool-call
// blocks (their result is filled in later from the correlated toolResult).
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
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
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
		case "toolCall":
			args := ""
			if len(blk.Arguments) > 0 {
				args = string(blk.Arguments)
			}
			out = append(out, ToolCallBlock(blk.ID, blk.Name, args))
		}
	}
	return out
}

// extractText concatenates the text of a pi message content field (used for a
// toolResult's content, an array of text/image blocks — only text is surfaced).
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
