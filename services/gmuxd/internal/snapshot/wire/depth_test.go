package wire

import (
	"encoding/json"
	"strings"
	"testing"

	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
)

func withDepth(n int) func(*central.SessionRow) {
	return func(r *central.SessionRow) { r.Depth = n }
}

// The durable projection resolves lineage depth over the full parent graph;
// the converter's job is to surface that value on local rows — an explicit 0
// for a root — and leave foreign network-peer rows nil (omitted on the wire).
// Depth-walk correctness (chains, dangling, dismissed ancestors, cycles) is
// pinned in the centralstore snapshot/lineage tests, not here.
func TestSessionsCopyProjectionDepth(t *testing.T) {
	local := []central.SessionRow{
		localRow("root", true, withDepth(0)),
		localRow("child", true, withDepth(1)),
		localRow("grand", true, withDepth(2)),
	}
	peers := []Session{{ID: "remote@peer", Peer: "peer", Adapter: "shell"}}

	got := (&Converter{}).Sessions(
		&central.SessionsPayload{Sessions: local},
		&central.ProjectsPayload{},
		peers,
	)

	wantDepth := map[string]int{"root": 0, "child": 1, "grand": 2}
	byID := map[string]Session{}
	for _, s := range got.Sessions {
		byID[s.ID] = s
	}
	for id, want := range wantDepth {
		s := byID[id]
		if s.Depth == nil {
			t.Fatalf("session %q: Depth is nil, want %d", id, want)
		}
		if *s.Depth != want {
			t.Errorf("session %q depth = %d, want %d", id, *s.Depth, want)
		}
	}

	if d := byID["remote@peer"].Depth; d != nil {
		t.Errorf("peer row Depth = %d, want nil/omitted", *d)
	}

	rootJSON, _ := json.Marshal(byID["root"])
	if !strings.Contains(string(rootJSON), `"depth":0`) {
		t.Errorf("root JSON missing explicit depth 0: %s", rootJSON)
	}
	peerJSON, _ := json.Marshal(byID["remote@peer"])
	if strings.Contains(string(peerJSON), `"depth"`) {
		t.Errorf("peer JSON should omit depth: %s", peerJSON)
	}
}
