package naming

import (
	"crypto/rand"
	"fmt"
)

// SessionID generates a unique session identifier.
func SessionID() string {
	return "sess-" + shortID()
}

func shortID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
