package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/projectmatch"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/tsauth"
)

// centralPeerAdapter is the concrete S5 selection seam. All accessors are
// copy-only snapshots; callbacks do durable work only after Manager releases
// its locks. It deliberately has no legacy store.Store reference.
type centralPeerAdapter struct {
	manager   *peering.Manager
	store     *centralstore.Store
	dirty     func(bool, bool)
	health    func() central.HealthInfo
	tailscale *tsauth.Listener
	now       func() centralstore.UnixMillis
}

func (a *centralPeerAdapter) LocalPeerMatchInputs() []centralstore.LocalPeerMatchInput {
	rows := a.manager.SessionProjections()
	out := make([]centralstore.LocalPeerMatchInput, 0, len(rows))
	for _, s := range rows {
		if s.Peer == "" || !a.manager.IsLocalPeer(s.Peer) {
			continue
		}
		out = append(out, centralstore.LocalPeerMatchInput{Subject: centralstore.LocalPeerSubject{PeerKey: centralstore.PeerKey(s.Peer), SessionID: peeringOriginalID(s.ID)}, CWD: s.Cwd, WorkspaceRoot: s.WorkspaceRoot, Remotes: copyStringMap(s.Remotes)})
	}
	return out
}
func peeringOriginalID(id string) string { orig, _ := peering.ParseID(id); return orig }
func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (a *centralPeerAdapter) PeerSessions() []wire.Session {
	rows := a.manager.SessionProjections()
	out := make([]wire.Session, 0, len(rows))
	for _, r := range rows {
		b, _ := json.Marshal(r)
		var s wire.Session
		if json.Unmarshal(b, &s) == nil {
			out = append(out, s)
		}
	}
	return out
}
func (a *centralPeerAdapter) PeerWorld() central.PeerWorld {
	w := a.manager.WorldProjection()
	h := a.health()
	if a.tailscale != nil {
		d := a.tailscale.Diag()
		h.Tailscale = &d
	}
	return central.PeerWorld{Peers: w.Peers, Health: &h, PeerProjects: w.PeerProjects, PeerDiscovered: w.PeerDiscovered, LocalPeerSessions: localPresence(a.manager.SessionProjections(), a.manager)}
}
func localPresence(rows []peering.SessionProjection, m *peering.Manager) map[central.LocalPeerSessionKey]struct{} {
	out := map[central.LocalPeerSessionKey]struct{}{}
	for _, s := range rows {
		if m.IsLocalPeer(s.Peer) {
			out[central.LocalPeerSessionKey{PeerKey: centralstore.PeerKey(s.Peer), SessionID: peeringOriginalID(s.ID)}] = struct{}{}
		}
	}
	return out
}
func (a *centralPeerAdapter) hooks() peering.EventHooks {
	return peering.EventHooks{
		PeerWorldDirty: func() { a.dirty(false, true) }, PeerSessionsDirty: func() { a.dirty(true, false) },
		LocalPeerConnected: func(_ string, rows []peering.SessionProjection) { a.assign(rows) },
		LocalPeerDisconnected: func(name string) {
			r, err := a.store.PruneLocalPeer(context.Background(), centralstore.PeerKey(name))
			if err == nil && r.Changed {
				a.dirty(r.SessionsDirty, r.WorldDirty)
			}
		},
	}
}
func (a *centralPeerAdapter) assign(rows []peering.SessionProjection) {
	ctx := context.Background()
	snap, err := a.store.ReadSnapshot(ctx, centralstore.SnapshotQuery{IncludeProjects: true})
	if err != nil {
		return
	}
	entries := make([]projectmatch.Entry, 0, len(snap.Projects))
	ids := make([]centralstore.ProjectEntryID, 0, len(snap.Projects))
	for _, p := range snap.Projects {
		if p.Kind != centralstore.ProjectEntryOwned {
			continue
		}
		e := projectmatch.Entry{}
		for _, r := range p.Rules {
			e.Rules = append(e.Rules, projectmatch.Rule{Path: r.Path, Remote: r.Remote, Exact: r.Exact})
		}
		entries = append(entries, e)
		ids = append(ids, p.ID)
	}
	var combined centralstore.MutationResult
	for _, s := range rows {
		i, ok := projectmatch.Match(entries, projectmatch.Inputs{CWD: s.Cwd, WorkspaceRoot: s.WorkspaceRoot, Remotes: s.Remotes})
		if !ok {
			continue
		}
		r, e := a.store.UpsertLocalPeerPlacement(ctx, centralstore.LocalPeerSubject{PeerKey: centralstore.PeerKey(s.Peer), SessionID: peeringOriginalID(s.ID), ParentSessionID: peeringOriginalID(s.ParentSessionID)}, ids[i])
		if e != nil {
			continue
		}
		combined.Changed = combined.Changed || r.Changed
		combined.SessionsDirty = combined.SessionsDirty || r.SessionsDirty
		combined.WorldDirty = combined.WorldDirty || r.WorldDirty
	}
	if combined.Changed {
		a.dirty(combined.SessionsDirty, combined.WorldDirty)
	}
}

// reconcileManualPeers converges runtime only after the durable commit.
func reconcileManualPeers(ctx context.Context, st *centralstore.Store, mgr *peering.Manager) error {
	want, err := st.ListManualPeers(ctx)
	if err != nil {
		return err
	}
	desired := map[string]centralstore.ManualPeer{}
	for _, p := range want {
		desired[p.Name] = p
	}
	for _, info := range mgr.PeerStatus() {
		p, ok := desired[info.Name]
		if !ok {
			mgr.RemovePeer(info.Name)
			continue
		}
		live := mgr.GetPeer(info.Name)
		if live.Config.URL != p.URL || live.Config.Token != p.Token {
			mgr.RemovePeer(info.Name)
			mgr.AddPeer(config.PeerConfig{Name: p.Name, URL: p.URL, Token: p.Token})
		}
		delete(desired, info.Name)
	}
	for _, p := range desired {
		if p.Name == "" {
			return fmt.Errorf("manual peer has empty name")
		}
		mgr.AddPeer(config.PeerConfig{Name: p.Name, URL: p.URL, Token: p.Token})
	}
	return nil
}

var _ central.PeerSource = (*centralPeerAdapter)(nil)
var _ wire.PeerSessionSource = (*centralPeerAdapter)(nil)
