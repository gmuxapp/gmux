package naming

import (
	"regexp"
	"strings"
	"testing"
)

func TestSessionIDShapeAndDigitGuarantee(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9a-z]{8}$`)
	seen := make(map[string]struct{})
	for range 1000 {
		id := SessionID()
		if !shape.MatchString(id) {
			t.Fatalf("SessionID() = %q, want 8 lowercase base36 characters", id)
		}
		if !strings.ContainsAny(id, "0123456789") {
			t.Fatalf("SessionID() = %q, want at least one digit", id)
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate SessionID() in test sample: %q", id)
		}
		seen[id] = struct{}{}
	}
}
