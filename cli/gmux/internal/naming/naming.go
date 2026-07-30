package naming

import "crypto/rand"

const (
	sessionIDLength = 8
	base36Alphabet  = "0123456789abcdefghijklmnopqrstuvwxyz"
)

// SessionID generates an opaque lowercase base36 session identifier. Rejection
// sampling avoids modulo bias, and the digit guarantee keeps bare IDs visually
// distinct from ordinary words now that session IDs have no textual prefix.
func SessionID() string {
	for {
		id := make([]byte, sessionIDLength)
		hasDigit := false
		for i := range id {
			var sample [1]byte
			for {
				if _, err := rand.Read(sample[:]); err != nil {
					panic("naming: crypto/rand failed: " + err.Error())
				}
				if sample[0] < 252 { // largest multiple of 36 below 256
					break
				}
			}
			id[i] = base36Alphabet[int(sample[0])%len(base36Alphabet)]
			if id[i] >= '0' && id[i] <= '9' {
				hasDigit = true
			}
		}
		if hasDigit {
			return string(id)
		}
	}
}
