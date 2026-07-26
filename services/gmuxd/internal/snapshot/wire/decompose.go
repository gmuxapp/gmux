package wire

import (
	"sort"
	"strings"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
)

// ScopeOrder is one sibling-scope reorder derived from a flat wire order:
// the input to centralstore.ReorderSiblings. Order always contains every
// current sibling of the scope (the domain op's contract).
type ScopeOrder struct {
	Project centralstore.ProjectEntryID
	Parent  centralstore.ParentRef
	Order   []centralstore.SubjectRef
}

// reorderTarget is one resolved participant of the project during
// decomposition.
type reorderTarget struct {
	subject centralstore.SubjectRef
	scope   string
	pos     int
}

// DecomposeReorder is the write-side inverse of the FD-1 flatten: it turns
// the legacy PATCH /v1/projects/{slug}/sessions flat key list back into
// per-sibling-scope orders (design §3.2 — sibling moves only; cross-scope
// moves are promotion ops and never produced here).
//
// Key handling preserves today's contract (design L-2 as corrected by the
// fable review M-2 — production's PATCH handler filters request keys
// against session IDs only, and v4 projects.json is IDs-only; the design's
// original PATCH row miscited "slug keys", which the frontend never sends):
//
//   - plain local session IDs;
//   - Local-peer namespaced "id@peer" keys (parent-owned placement);
//   - anything else — including slug keys — is silently dropped, exactly
//     like production's known-ID filter. That also covers keys naming
//     sessions not placed in this project: the old ReorderSessions merge
//     could implicitly ADD such keys to projects.json; the durable model
//     separates placement from ordering, so a reorder never places
//     (deviation recorded in the S2 report).
//
// Deliberate deviation (fable L-2): duplicate request keys are deduped
// first-mention-wins. Production's merge wrote duplicates into
// projects.json verbatim (a latent corruption); ReorderSiblings rejects
// duplicate subjects, so the dedup is both required and strictly better.
//
// Keys the request omits keep their relative order at the tail of their
// scope (production merge parity). The returned orders cover only scopes
// the request actually mentions; ok is false when slug names no owned
// project entry (the route's 404).
func (c *Converter) DecomposeReorder(slug string, keys []string, local *central.SessionsPayload, world *central.ProjectsPayload) ([]ScopeOrder, bool) {
	if world == nil {
		return nil, false
	}
	var project centralstore.ProjectEntryID
	found := false
	for _, e := range world.Projects {
		if e.Slug == slug && e.Kind == centralstore.ProjectEntryOwned {
			project, found = e.ID, true
			break
		}
	}
	if !found {
		return nil, false
	}

	// Participant tables for this project.
	byLocalID := map[string]reorderTarget{}
	if local != nil {
		for _, row := range local.Sessions {
			p := row.Placement
			if p == nil || p.ProjectSlug != slug {
				continue
			}
			byLocalID[string(row.ID)] = reorderTarget{
				subject: centralstore.SubjectRef{LocalSessionID: row.ID},
				scope:   p.SiblingScope, pos: p.Position,
			}
		}
	}
	byNamespaced := map[string]reorderTarget{}
	lpParent := map[string]string{} // nodeKey → durable ParentSessionID (for parent refs)
	for _, p := range worldPlacements(world) {
		// Parent linkage is recorded for every placement (a Local-peer
		// parent scope key must resolve even when only its children are
		// mentioned).
		nodeKey := "p:" + escapeScope(string(p.PeerKey)) + ":" + escapeScope(p.SessionID)
		lpParent[nodeKey] = p.ParentSessionID
		if p.ProjectSlug != slug {
			continue
		}
		nsKey := peering.NamespaceID(p.SessionID, string(p.PeerKey))
		t := reorderTarget{
			subject: centralstore.SubjectRef{LocalPeer: &centralstore.LocalPeerSubject{
				PeerKey: p.PeerKey, SessionID: p.SessionID, ParentSessionID: p.ParentSessionID,
			}},
			scope: p.SiblingScope, pos: p.Position,
		}
		byNamespaced[nsKey] = t
	}

	// Resolve keys in request order; group mentions per current scope.
	mentioned := map[string][]reorderTarget{}
	seen := map[string]bool{}
	for _, key := range keys {
		t, ok := byLocalID[key]
		if !ok {
			t, ok = byNamespaced[key]
		}
		if !ok {
			continue // unknown key (incl. slug keys): silently dropped (L-2/M-2)
		}
		sk := subjectKeyOf(t.subject)
		if seen[sk] {
			continue // duplicate key: first mention wins (deliberate, fable L-2)
		}
		seen[sk] = true
		mentioned[t.scope] = append(mentioned[t.scope], t)
	}
	if len(mentioned) == 0 {
		return nil, true
	}

	// Full sibling lists per scope, in current durable order.
	siblings := map[string][]reorderTarget{}
	for _, t := range byLocalID {
		siblings[t.scope] = append(siblings[t.scope], t)
	}
	for _, t := range byNamespaced {
		siblings[t.scope] = append(siblings[t.scope], t)
	}
	for _, s := range siblings {
		sort.Slice(s, func(i, j int) bool { return s[i].pos < s[j].pos })
	}

	scopes := make([]string, 0, len(mentioned))
	for scope := range mentioned {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)

	out := make([]ScopeOrder, 0, len(scopes))
	for _, scope := range scopes {
		parent, ok := parentRefOf(scope, lpParent)
		if !ok {
			continue // unparseable scope: never emitted by the store
		}
		order := make([]centralstore.SubjectRef, 0, len(siblings[scope]))
		for _, t := range mentioned[scope] {
			order = append(order, t.subject)
		}
		for _, t := range siblings[scope] {
			if !seen[subjectKeyOf(t.subject)] {
				order = append(order, t.subject)
			}
		}
		out = append(out, ScopeOrder{Project: project, Parent: parent, Order: order})
	}
	return out, true
}

// parentRefOf reconstructs the ReorderSiblings parent reference from a
// durable sibling-scope string: "r" (roots), "c:l:<id>" (local parent) or
// "c:p:<peer>:<session>" (Local-peer parent, durable escaping).
func parentRefOf(scope string, lpParent map[string]string) (centralstore.ParentRef, bool) {
	if scope == "r" {
		return centralstore.ParentRef{}, true
	}
	rest, ok := strings.CutPrefix(scope, "c:")
	if !ok {
		return centralstore.ParentRef{}, false
	}
	if id, ok := strings.CutPrefix(rest, "l:"); ok && id != "" {
		return centralstore.ParentRef{Subject: &centralstore.SubjectRef{LocalSessionID: centralstore.SessionID(id)}}, true
	}
	body, ok := strings.CutPrefix(rest, "p:")
	if !ok {
		return centralstore.ParentRef{}, false
	}
	peer, sess, ok := strings.Cut(body, ":")
	if !ok || peer == "" || sess == "" {
		return centralstore.ParentRef{}, false
	}
	return centralstore.ParentRef{Subject: &centralstore.SubjectRef{LocalPeer: &centralstore.LocalPeerSubject{
		PeerKey:         centralstore.PeerKey(unescapeScope(peer)),
		SessionID:       unescapeScope(sess),
		ParentSessionID: lpParent[rest],
	}}}, true
}

func subjectKeyOf(s centralstore.SubjectRef) string {
	if s.LocalSessionID != "" {
		return "l:" + string(s.LocalSessionID)
	}
	return "p:" + escapeScope(string(s.LocalPeer.PeerKey)) + ":" + escapeScope(s.LocalPeer.SessionID)
}
