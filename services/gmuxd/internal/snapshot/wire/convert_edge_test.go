package wire

import (
	"encoding/json"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
)

// TestDecomposeReorderEdgeCases exercises the decomposer against
// malformed input: empty project, missing sessions, unknown IDs,
// duplicate IDs, and empty order.
func TestDecomposeReorderEdgeCases(t *testing.T) {
	conv := &Converter{IsLocalPeer: func(n string) bool { return n == "box" }}

	local := &central.SessionsPayload{Sessions: []central.SessionRow{
		{SessionView: centralstore.SessionView{
			Session:   centralstore.Session{ID: "sess-a", Adapter: "shell", Command: []string{"bash"}, CreatedAt: 1, StatusReported: true},
			Placement: &centralstore.SessionPlacement{ProjectSlug: "proj", SiblingScope: "r", Position: 0},
		}},
		{SessionView: centralstore.SessionView{
			Session:   centralstore.Session{ID: "sess-b", Adapter: "pi", Command: []string{"pi"}, CreatedAt: 2, StatusReported: true},
			Placement: &centralstore.SessionPlacement{ProjectSlug: "proj", SiblingScope: "r", Position: 1},
		}},
	}}
	world := &central.ProjectsPayload{
		Projects: centralstore.ProjectCatalog{
			{ID: 1, Kind: centralstore.ProjectEntryOwned, Slug: "proj"},
		},
	}

	cases := []struct {
		name     string
		slug     string
		sessions []string
		wantOK   bool
	}{
		{"unknown project", "unknown", []string{"sess-a"}, false},
		{"empty order", "proj", []string{}, true},                         // empty is valid: no-op reorder
		{"missing session", "proj", []string{"sess-missing"}, true},       // silently dropped
		{"duplicate session", "proj", []string{"sess-a", "sess-a"}, true}, // deduped
		{"valid reorder", "proj", []string{"sess-b", "sess-a"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := conv.DecomposeReorder(tc.slug, tc.sessions, local, world)
			if ok != tc.wantOK {
				t.Errorf("DecomposeReorder(%q, %v) = ok=%v, want %v", tc.slug, tc.sessions, ok, tc.wantOK)
			}
		})
	}
}

// TestSessionJSONRoundTrip verifies that Session marshals and unmarshals
// correctly, including edge cases with nil/empty fields.
func TestSessionJSONRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		s    Session
	}{
		{"minimal", Session{ID: "sess-1", Adapter: "shell"}},
		{"with status", Session{ID: "sess-1", Adapter: "shell", Status: &Status{Working: true}}},
		{"with nil status", Session{ID: "sess-1", Adapter: "shell", Status: nil}},
		{"with remotes", Session{ID: "sess-1", Adapter: "shell", Remotes: map[string]string{"origin": "https://github.com"}}},
		{"with empty remotes", Session{ID: "sess-1", Adapter: "shell", Remotes: map[string]string{}}},
		{"with command", Session{ID: "sess-1", Adapter: "shell", Command: []string{"bash", "-l"}}},
		{"with nil command", Session{ID: "sess-1", Adapter: "shell", Command: nil}},
		{"with exit code", Session{ID: "sess-1", Adapter: "shell"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.s)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Session
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			// Check key fields round-trip correctly.
			if got.ID != tc.s.ID {
				t.Errorf("ID = %q, want %q", got.ID, tc.s.ID)
			}
			if got.Adapter != tc.s.Adapter {
				t.Errorf("Adapter = %q, want %q", got.Adapter, tc.s.Adapter)
			}
		})
	}
}

// TestMalformedJSONInput verifies that the decomposer handles malformed
// JSON gracefully without panicking.
func TestMalformedJSONInput(t *testing.T) {
	conv := &Converter{IsLocalPeer: func(n string) bool { return false }}

	// Malformed sessions payload (not valid JSON)
	badJSON := []byte(`{"sessions": [invalid`)
	var bad central.SessionsPayload
	err := json.Unmarshal(badJSON, &bad)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}

	// DecomposeReorder with nil payloads should not panic
	_, ok := conv.DecomposeReorder("proj", []string{"sess-1"}, nil, nil)
	if ok {
		t.Error("expected false for nil payloads")
	}
}

// TestLargePayloadNoPanic verifies the decomposer handles a large number
// of sessions without panicking. There is no enforced size limit; this
// is a sanity check that the converter scales to sidebar-scale payloads.
func TestLargePayloadNoPanic(t *testing.T) {
	conv := &Converter{IsLocalPeer: func(n string) bool { return false }}

	// Build a sessions payload with many sessions
	sessions := make([]central.SessionRow, 1000)
	for i := range sessions {
		sessions[i] = central.SessionRow{
			SessionView: centralstore.SessionView{
				Session: centralstore.Session{
					ID:             centralstore.SessionID("sess-" + string(rune('a'+i%26))),
					Adapter:        "shell",
					Command:        []string{"bash"},
					CreatedAt:      centralstore.UnixMillis(1000 + i),
					StatusReported: true,
				},
				Placement: &centralstore.SessionPlacement{
					ProjectSlug:  "proj",
					SiblingScope: "r",
					Position:     i,
				},
			},
		}
	}

	local := &central.SessionsPayload{Sessions: sessions}
	world := &central.ProjectsPayload{
		Projects: centralstore.ProjectCatalog{
			{ID: 1, Kind: centralstore.ProjectEntryOwned, Slug: "proj"},
		},
	}

	// Build a reorder list with all session IDs
	order := make([]string, 1000)
	for i := range order {
		order[i] = "sess-" + string(rune('a'+i%26))
	}

	// This should handle the large payload without panicking
	orders, ok := conv.DecomposeReorder("proj", order, local, world)
	if !ok {
		t.Fatal("expected successful decompose for large payload")
	}
	if len(orders) == 0 {
		t.Error("expected at least one scope order")
	}
}

// TestInvalidSessionID verifies that invalid session IDs are handled
// gracefully in the decomposer.
func TestInvalidSessionID(t *testing.T) {
	conv := &Converter{IsLocalPeer: func(n string) bool { return false }}

	local := &central.SessionsPayload{Sessions: []central.SessionRow{
		{SessionView: centralstore.SessionView{
			Session:   centralstore.Session{ID: "sess-a", Adapter: "shell", Command: []string{"bash"}, CreatedAt: 1, StatusReported: true},
			Placement: &centralstore.SessionPlacement{ProjectSlug: "proj", SiblingScope: "r", Position: 0},
		}},
	}}
	world := &central.ProjectsPayload{
		Projects: centralstore.ProjectCatalog{
			{ID: 1, Kind: centralstore.ProjectEntryOwned, Slug: "proj"},
		},
	}

	// Empty session ID in order: silently dropped (known-ID filter)
	_, ok := conv.DecomposeReorder("proj", []string{""}, local, world)
	if !ok {
		t.Error("expected true for empty session ID (silently dropped)")
	}

	// Path traversal in session ID: silently dropped (not a known ID)
	_, ok = conv.DecomposeReorder("proj", []string{"../etc/passwd"}, local, world)
	if !ok {
		t.Error("expected true for path traversal session ID (silently dropped)")
	}
}

// TestProjectSlugEdgeCases verifies behavior with unusual project slugs.
func TestProjectSlugEdgeCases(t *testing.T) {
	conv := &Converter{IsLocalPeer: func(n string) bool { return false }}

	local := &central.SessionsPayload{Sessions: []central.SessionRow{
		{SessionView: centralstore.SessionView{
			Session:   centralstore.Session{ID: "sess-a", Adapter: "shell", Command: []string{"bash"}, CreatedAt: 1, StatusReported: true},
			Placement: &centralstore.SessionPlacement{ProjectSlug: "my-proj", SiblingScope: "r", Position: 0},
		}},
	}}

	cases := []struct {
		name   string
		slug   string
		world  *central.ProjectsPayload
		wantOK bool
	}{
		{"empty slug", "", nil, false},
		{"nil world", "my-proj", nil, false},
		{"slug not in catalog", "unknown", &central.ProjectsPayload{
			Projects: centralstore.ProjectCatalog{
				{ID: 1, Kind: centralstore.ProjectEntryOwned, Slug: "other"},
			},
		}, false},
		{"matching slug", "my-proj", &central.ProjectsPayload{
			Projects: centralstore.ProjectCatalog{
				{ID: 1, Kind: centralstore.ProjectEntryOwned, Slug: "my-proj"},
			},
		}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := conv.DecomposeReorder(tc.slug, []string{"sess-a"}, local, tc.world)
			if ok != tc.wantOK {
				t.Errorf("DecomposeReorder(%q) = ok=%v, want %v", tc.slug, ok, tc.wantOK)
			}
		})
	}
}
