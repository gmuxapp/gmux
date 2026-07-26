package centralstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestManualPeerAddSlugifiesAndDecollides(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)

	p, outcome, r, err := s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "My Laptop!", URL: "https://laptop:7369", Token: "sec-1"}, 5)
	if err != nil || outcome != PeerAdded || !r.Changed || !r.WorldDirty || r.SessionsDirty {
		t.Fatalf("peer=%#v outcome=%v result=%#v err=%v", p, outcome, r, err)
	}
	if p.Name != "my-laptop" || p.Token != "sec-1" || p.CreatedAt != 5 || p.UpdatedAt != 5 || p.Version != 1 {
		t.Fatalf("peer=%#v", p)
	}

	// A different host with a colliding name gets a suffixed slug.
	p2, outcome, _, err := s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "my laptop", URL: "https://other:7369", NodeID: "node-b"}, 6)
	if err != nil || outcome != PeerAdded || p2.Name != "my-laptop-2" {
		t.Fatalf("peer2=%#v outcome=%v err=%v", p2, outcome, err)
	}

	list, err := s.ListManualPeers(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "my-laptop" || list[1].Name != "my-laptop-2" {
		t.Fatalf("list=%#v err=%v", list, err)
	}
}

func TestManualPeerUpsertMatchesByNodeIDThenURL(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	orig, _, _, err := s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "box", URL: "https://box:7369", Token: "tok", NodeID: "node-a"}, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Same node_id reached at a new address: URL refreshed, name kept, no
	// duplicate.
	p, outcome, r, err := s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "Renamed Box", URL: "https://box.tailnet:7369", NodeID: "node-a"}, 2)
	if err != nil || outcome != PeerUpdated || !r.Changed || !r.WorldDirty {
		t.Fatalf("peer=%#v outcome=%v result=%#v err=%v", p, outcome, r, err)
	}
	if p.Name != "box" || p.URL != "https://box.tailnet:7369" || p.Token != "tok" || p.Version != orig.Version+1 || p.UpdatedAt != 2 {
		t.Fatalf("peer=%#v", p)
	}

	// URL match (trailing slash / case insensitive) when node_id unknown:
	// non-empty token/node_id refresh in place.
	p, outcome, _, err = s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "whatever", URL: "https://BOX.tailnet:7369/", Token: "tok-2", NodeID: "node-a2"}, 3)
	if err != nil || outcome != PeerUpdated {
		t.Fatalf("peer=%#v outcome=%v err=%v", p, outcome, err)
	}
	if p.Name != "box" || p.Token != "tok-2" || p.NodeID != "node-a2" {
		t.Fatalf("peer=%#v", p)
	}

	list, err := s.ListManualPeers(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%#v err=%v", list, err)
	}
}

// TestManualPeerEmptyMeansUnknownNotClear: production parity with
// peerstore.AddOrGet — a token-less/id-less re-add must not wipe stored
// values, and an identical re-add is an unchanged no-op.
func TestManualPeerEmptyMeansUnknownNotClear(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	if _, _, _, err := s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "box", URL: "https://box:7369", Token: "tok", NodeID: "node-a"}, 1); err != nil {
		t.Fatal(err)
	}
	p, outcome, r, err := s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "box", URL: "https://box:7369"}, 2)
	if err != nil || outcome != PeerUnchanged || r.Changed || r.WorldDirty {
		t.Fatalf("peer=%#v outcome=%v result=%#v err=%v", p, outcome, r, err)
	}
	if p.Token != "tok" || p.NodeID != "node-a" || p.UpdatedAt != 1 {
		t.Fatalf("unknown cleared stored values: %#v", p)
	}
}

func TestManualPeerValidationAndRemoval(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cases := []ManualPeerSpec{
		{Name: "box", URL: "ftp://box"},        // bad scheme
		{Name: "box", URL: "https://"},         // no host
		{Name: "!!!", URL: "https://box:7369"}, // unusable name
		{Name: "box", URL: "://not a url"},     // unparseable
	}
	for _, spec := range cases {
		if _, _, _, err := s.UpsertManualPeer(ctx, spec, 1); err == nil {
			t.Fatalf("invalid spec accepted: %#v", spec)
		}
	}
	if _, _, _, err := s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "box", URL: "https://box:7369"}, -1); err == nil {
		t.Fatal("negative timestamp accepted")
	}
	if list, err := s.ListManualPeers(ctx); err != nil || len(list) != 0 {
		t.Fatalf("failed upserts persisted rows: %#v err=%v", list, err)
	}

	if _, err := s.RemoveManualPeer(ctx, "missing"); !errors.Is(err, ErrPeerNotFound) {
		t.Fatalf("remove missing err=%v", err)
	}
	if _, _, _, err := s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "box", URL: "https://box:7369", Token: "tok"}, 1); err != nil {
		t.Fatal(err)
	}
	r, err := s.RemoveManualPeer(ctx, "box")
	if err != nil || !r.Changed || !r.WorldDirty {
		t.Fatalf("remove result=%#v err=%v", r, err)
	}
	if list, err := s.ListManualPeers(ctx); err != nil || len(list) != 0 {
		t.Fatalf("list after remove=%#v err=%v", list, err)
	}
}

// TestManualPeerRedactionSeam: the export projection elides the token value
// and reports only its presence; peers survive daemon restart (reopen).
func TestManualPeerRedactionSeamAndDurability(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	p, _, _, err := s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "box", URL: "https://box:7369", Token: "super-secret", NodeID: "node-a"}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, err := s.ListManualPeers(ctx)
	if err != nil || len(list) != 1 || list[0] != p {
		t.Fatalf("peer did not survive reopen: %#v err=%v", list, err)
	}
	red := p.Redacted()
	if !red.TokenPresent || red.Name != "box" || red.URL != "https://box:7369" || red.NodeID != "node-a" || red.CreatedAt != 7 {
		t.Fatalf("redacted=%#v", red)
	}
	// Structural guarantee: the redacted type has no field containing the
	// secret value.
	if strings.Contains(fmt.Sprintf("%#v", red), "super-secret") {
		t.Fatalf("redacted projection leaks token: %#v", red)
	}

	tokenless := ManualPeer{Name: "n", URL: "u"}
	if tokenless.Redacted().TokenPresent {
		t.Fatal("token presence fabricated")
	}
}
