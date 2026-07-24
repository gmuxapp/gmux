package main

import (
	"encoding/json"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// TestFanoutCopyPreservesEmptySlices verifies that the fanout deep-copy
// helpers never degrade an empty [] to null — the nil-vs-empty bug class.
func TestFanoutCopyPreservesEmptySlices(t *testing.T) {
	f := newSSEFanout()

	// Broadcast frames with explicitly empty (non-nil) slices.
	f.BroadcastFrames(wire.Frames{
		Sessions: &wire.SessionsPayload{Sessions: []wire.Session{}},
		World: &wire.WorldPayload{
			Projects:  []wire.ProjectItem{},
			Peers:     nil, // deliberately nil — copy must normalize
			Launchers: nil,
		},
	})

	frames := f.Current()
	if frames.Sessions == nil {
		t.Fatal("sessions frame is nil")
	}
	if frames.World == nil {
		t.Fatal("world frame is nil")
	}

	// Marshal and check for null array fields.
	for label, v := range map[string]any{
		"sessions": frames.Sessions,
		"world":    frames.World,
	} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", label, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: unmarshal: %v", label, err)
		}
		// health is a *HealthInfo pointer; null is correct ("no health").
		// peer_projects/peer_discovered are omitempty maps.
		allowNull := map[string]bool{"health": true, "status": true}
		for key, val := range m {
			if allowNull[key] {
				continue
			}
			if string(val) == "null" {
				t.Errorf("%s.%s is null, want [] — nil-vs-empty bug class", label, key)
			}
		}
	}
}
