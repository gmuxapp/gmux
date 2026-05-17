package snapshot

import (
	"encoding/json"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

func TestComposeSessions_NoFilterIncludesAll(t *testing.T) {
	in := []store.Session{
		{ID: "a", Peer: ""},
		{ID: "b", Peer: "tower"},
		{ID: "c", Peer: "laptop"},
	}
	got := ComposeSessions(in, nil)
	if len(got.Sessions) != 3 {
		t.Fatalf("len = %d, want 3 (no filter), got %+v", len(got.Sessions), got.Sessions)
	}
}

func TestComposeSessions_FilterDropsUnowned(t *testing.T) {
	in := []store.Session{
		{ID: "local-1", Peer: ""},
		{ID: "tower-1", Peer: "tower"},
		{ID: "laptop-1", Peer: "laptop"},
	}
	// Owned = local + tower; laptop is a network peer being filtered out.
	owned := func(s *store.Session) bool {
		return s.Peer == "" || s.Peer == "tower"
	}
	got := ComposeSessions(in, owned)
	ids := make([]string, len(got.Sessions))
	for i, s := range got.Sessions {
		ids[i] = s.ID
	}
	want := map[string]bool{"local-1": true, "tower-1": true}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected id %q in payload", id)
		}
	}
}

func TestComposeSessions_EmptyInputProducesEmptySlice(t *testing.T) {
	got := ComposeSessions(nil, nil)
	if got.Sessions == nil {
		t.Fatal("Sessions slice should be non-nil for stable JSON")
	}
	if len(got.Sessions) != 0 {
		t.Errorf("len = %d, want 0", len(got.Sessions))
	}
	// Marshals to [] not null, so the frontend can blindly Array.from it.
	b, _ := json.Marshal(got)
	if string(b) != `{"sessions":[]}` {
		t.Errorf("json = %s, want {\"sessions\":[]}", b)
	}
}

func TestComposeSessions_PreservesInputOrder(t *testing.T) {
	// Order is part of the protocol: clients can stable-sort or
	// rely on origin order without a tiebreaker.
	in := []store.Session{{ID: "z"}, {ID: "a"}, {ID: "m"}}
	got := ComposeSessions(in, nil)
	if got.Sessions[0].ID != "z" || got.Sessions[1].ID != "a" || got.Sessions[2].ID != "m" {
		t.Errorf("order changed: %+v", got.Sessions)
	}
}

func TestWorldPayload_MarshalsAllFields(t *testing.T) {
	p := WorldPayload{
		Projects:        []map[string]any{{"slug": "gmux"}},
		Peers:           []map[string]any{{"name": "tower", "status": "connected"}},
		Health:          map[string]any{"hostname": "node-a"},
		Launchers:       []map[string]any{{"id": "shell"}},
		DefaultLauncher: "shell",
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"projects", "peers", "health", "launchers", "default_launcher"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q in %s", k, b)
		}
	}
}
