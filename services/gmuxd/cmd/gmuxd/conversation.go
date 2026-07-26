package main

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// formatConversationMarkdown renders adapter-neutral conversation messages.
func formatConversationMarkdown(msgs []adapter.ConversationMessage) []byte {
	var b bytes.Buffer
	for i, msg := range msgs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n", roleHeading(msg.Role), strings.TrimRight(msg.Text, "\n"))
	}
	return b.Bytes()
}

func roleHeading(role string) string {
	switch role {
	case "user":
		return "User"
	case "assistant":
		return "Assistant"
	case "":
		return "Message"
	default:
		return strings.ToUpper(role[:1]) + role[1:]
	}
}
