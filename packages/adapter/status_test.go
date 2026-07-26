package adapter

import (
	"encoding/json"
	"testing"
)

// TestStatusJSONShape pins the canonical status wire shape shared by the
// runner, gmuxd, peers and the frontend: active is always present, error
// and interrupted are omitted when false, and the three facts are
// orthogonal (an active error condition is representable).
func TestStatusJSONShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Status
		want string
	}{
		{"inactive", Status{}, `{"active":false}`},
		{"active", Status{Active: true}, `{"active":true}`},
		{"terminal error", Status{Error: true}, `{"active":false,"error":true}`},
		{"active error", Status{Active: true, Error: true}, `{"active":true,"error":true}`},
		{"interrupted", Status{Interrupted: true}, `{"active":false,"interrupted":true}`},
	} {
		b, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(b) != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, b, tc.want)
		}
		var back Status
		if err := json.Unmarshal(b, &back); err != nil || back != tc.in {
			t.Errorf("%s: round trip = %+v (%v)", tc.name, back, err)
		}
	}
}
