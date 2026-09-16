package wire

import (
	"encoding/base64"
	"sort"
	"strings"
)

// PROTO (3.0 world split): the sidebar's world state is bounded by SHAPE, not
// by count. The frame a browser subscribes to carries only presentation
// ROOTS; every root carries an additive summary of the subtree it stands in
// for, and the subtree itself is fetched on demand (GET /v1/sessions/{id}/
// children) and kept live by a scoped delta while it is being viewed.
//
// The family edge used here is the one the web UI has always used
// (family.ts potentialFamilyParent): a row is a family child iff its parent
// is present in the same payload AND that parent is a semantic agent. A
// missing parent, or a parent that is a shell/editor, leaves the row a root —
// which is why user-started shells (and the editor a shell spawned) keep
// their sidebar rows, and why a promoted child becomes a root in the same
// composer pass that removes it from its former parent's subtree.

// DescendantCounts is the additive per-root summary. Every field describes
// the root's descendants (any depth), the root itself excluded:
//
//   - Total / Alive: census.
//   - Unread: unread AGENT descendants — exactly the population the sidebar
//     badge used to count per child row (process output is task history,
//     not attention; store.ts unreadCountWith).
//   - Error / Waiting / Active / Running: the family activity buckets the
//     sidebar's family line renders (family.ts familyStateOf): an agent is
//     active when status.active, waiting(-error) when unread, error when
//     waiting with a durable error; a process contributes only while it is
//     running (alive ∧ status.active).
//
// Counts carried on a row that ARRIVES with them (a 3.0 spoke's roots-only
// feed) are added to whatever this host can see, so a hub renders a mixed
// fleet (2.1 spokes ship children, 3.0 spokes ship counts) with one rule.
type DescendantCounts struct {
	Total   int `json:"total"`
	Alive   int `json:"alive"`
	Unread  int `json:"unread"`
	Error   int `json:"error"`
	Waiting int `json:"waiting"`
	Active  int `json:"active"`
	Running int `json:"running"`
	// Children is the number of DIRECT children — what one page-through of
	// /children will enumerate. Total counts every depth.
	Children int `json:"children"`
}

func (c DescendantCounts) add(o DescendantCounts) DescendantCounts {
	c.Total += o.Total
	c.Alive += o.Alive
	c.Unread += o.Unread
	c.Error += o.Error
	c.Waiting += o.Waiting
	c.Active += o.Active
	c.Running += o.Running
	c.Children += o.Children
	return c
}

func (c DescendantCounts) isZero() bool { return c == DescendantCounts{} }

// contribution is what one descendant adds to every ancestor's summary.
func contribution(s Session) DescendantCounts {
	c := DescendantCounts{Total: 1}
	if s.Alive {
		c.Alive = 1
	}
	// Current activity, as the UI derives it: a dead row's durable
	// active-at-death fact (kept for gmux wait) is not live work.
	active := s.Alive && s.Status != nil && s.Status.Active
	if !s.SemanticAgent {
		if s.Alive && active {
			c.Running = 1
		}
		return c
	}
	if s.Unread {
		c.Unread = 1
	}
	switch {
	case active:
		c.Active = 1
	case s.Unread && s.Status != nil && s.Status.Error:
		c.Error = 1
	case s.Unread:
		c.Waiting = 1
	}
	return c
}

// FamilyIndex is the payload-wide family shape: which rows are children (and
// of whom), built once per payload with the UI's edge rule.
type FamilyIndex struct {
	byID     map[string]int
	parentOf map[string]string   // child id -> parent id (family edges only)
	children map[string][]string // parent id -> child ids
}

// IndexFamilies builds the family edges of one payload.
func IndexFamilies(rows []Session) *FamilyIndex {
	idx := &FamilyIndex{byID: make(map[string]int, len(rows)), parentOf: map[string]string{}, children: map[string][]string{}}
	for i, s := range rows {
		idx.byID[s.ID] = i
	}
	for _, s := range rows {
		if s.ParentSessionID == "" || s.ParentSessionID == s.ID {
			continue
		}
		pi, ok := idx.byID[s.ParentSessionID]
		if !ok || !rows[pi].SemanticAgent {
			continue
		}
		idx.parentOf[s.ID] = s.ParentSessionID
		idx.children[s.ParentSessionID] = append(idx.children[s.ParentSessionID], s.ID)
	}
	return idx
}

// IsChild reports whether id has a family parent in the indexed payload.
func (idx *FamilyIndex) IsChild(id string) bool { _, ok := idx.parentOf[id]; return ok }

// Children returns the direct child ids of id (unordered).
func (idx *FamilyIndex) Children(id string) []string { return idx.children[id] }

// AnnotateDescendantCounts stamps DescendantCounts on every row that has
// descendants in rows (and leaves rows without any untouched, so 99 % of the
// full payload is byte-identical to today). A row that arrives with counts
// but no visible children (a 3.0 spoke's root on a hub) keeps them; a row
// with visible children gets counts computed from them, REPLACING whatever it
// carried — so annotating twice is idempotent (round-2 L1). Cross-peer
// parentage is refused at registration, so "foreign counts plus local
// children" cannot occur; if it ever did, the local truth wins. Cycles (which
// the store rejects at registration, but a peer could ship) are cut by a
// visited set.
func AnnotateDescendantCounts(rows []Session) *FamilyIndex {
	idx := IndexFamilies(rows)
	memo := make(map[string]DescendantCounts, len(idx.children))
	var visiting map[string]bool
	var subtree func(id string) DescendantCounts
	subtree = func(id string) DescendantCounts {
		if c, ok := memo[id]; ok {
			return c
		}
		if visiting[id] {
			return DescendantCounts{}
		}
		if visiting == nil {
			visiting = map[string]bool{}
		}
		visiting[id] = true
		var c DescendantCounts
		kids := idx.children[id]
		for _, kid := range kids {
			c = c.add(contribution(rows[idx.byID[kid]]))
			c = c.add(subtree(kid))
		}
		c.Children = len(kids) // direct only; add() summed the kids' own Children
		delete(visiting, id)
		memo[id] = c
		return c
	}
	for i := range rows {
		id := rows[i].ID
		if len(idx.children[id]) == 0 {
			continue
		}
		c := subtree(id)
		if !c.isZero() {
			cc := c
			rows[i].DescendantCounts = &cc
		}
	}
	return idx
}

// RootsView is the browser's world state: rows that are not family children,
// each annotated with its subtree summary. rows must already be annotated
// (AnnotateDescendantCounts) — the view is a filter, so the two cannot drift.
// Order is preserved (ascending id, as the converter emits it).
//
// project_index is re-stamped as the dense rank among the ROOTS of each
// (peer, project) folder. The flat FD-1 index counts children too, so every
// subagent spawn renumbered every root placed after it in the project — on
// the live corpus a spawn under DRIVER touched 10 roots (8.5 KB) instead of
// one (0.9 KB). The sidebar uses project_index only as a sort key
// (projects.ts compareFolderSessions), and the rank among roots orders roots
// exactly as the flat index did, so no consumer of this view can tell the
// difference except by the bytes it no longer receives. Rows that carry the
// full index (2.x view, children pages) are untouched.
func RootsView(rows []Session, idx *FamilyIndex) []Session {
	out := make([]Session, 0, len(rows)/8+1)
	for _, s := range rows {
		if idx.IsChild(s.ID) {
			continue
		}
		out = append(out, s)
	}
	rerankRoots(out)
	return out
}

func rerankRoots(roots []Session) {
	type folder struct{ peer, slug string }
	byFolder := map[folder][]int{}
	for i, s := range roots {
		if s.ProjectSlug == "" {
			continue
		}
		f := folder{s.Peer, s.ProjectSlug}
		byFolder[f] = append(byFolder[f], i)
	}
	for _, members := range byFolder {
		sort.SliceStable(members, func(a, b int) bool {
			x, y := roots[members[a]], roots[members[b]]
			if x.ProjectIndex != y.ProjectIndex {
				return x.ProjectIndex < y.ProjectIndex
			}
			if x.CreatedAt != y.CreatedAt {
				return x.CreatedAt < y.CreatedAt
			}
			return x.ID < y.ID
		})
		for rank, i := range members {
			roots[i].ProjectIndex = rank
		}
	}
}

// RootsWorld narrows the world frame to match the roots view: a project's
// sessions[] lists only rows that have a sidebar row of their own.
func RootsWorld(w WorldPayload, idx *FamilyIndex) WorldPayload {
	items := make([]ProjectItem, len(w.Projects))
	for i, p := range w.Projects {
		q := p
		if len(p.Sessions) > 0 {
			q.Sessions = make([]string, 0, len(p.Sessions))
			for _, id := range p.Sessions {
				if !idx.IsChild(id) {
					q.Sessions = append(q.Sessions, id)
				}
			}
		}
		items[i] = q
	}
	w.Projects = items
	return w
}

// ── children pages ──

// ChildrenPage is GET /v1/sessions/{id}/children's data.
type ChildrenPage struct {
	SessionID  string    `json:"session_id"`
	Rows       []Session `json:"rows"`
	NextCursor string    `json:"next_cursor,omitempty"`
	// Total is the number of rows the whole listing (all pages) would
	// return at this epoch — direct children, or every descendant when the
	// listing was asked for descendants.
	Total int `json:"total"`
	// Descendants is true when Rows is the flattened subtree (any depth).
	Descendants bool `json:"descendants,omitempty"`
	// Epoch / BootID name the fanout state the page was cut from, so a client
	// can chain a scoped delta subscription onto it (or detect it must not).
	Epoch  uint64 `json:"epoch,omitempty"`
	BootID string `json:"boot_id,omitempty"`
}

// childOrder is the listing order: newest first by created_at (RFC 3339,
// fixed width, so string order is time order), then id descending.
//
// Cursor contract (round-2 M2, stated exactly):
//   - Cursors are opaque; clients echo next_cursor, never mint one from a row
//     (a hub re-namespaces rows but forwards the spoke's cursor as is).
//   - A keyset position is immutable, so paging never returns a row twice.
//   - A SPAWN is newer than every held row: it lands at the head and is never
//     skipped by later pages.
//   - A REPARENT/DEMOTE keeps the row's created_at and can land inside a range
//     already returned; later pages will not return it. A client with a live
//     scope over that range receives it as a window upsert; a client without
//     one (peer subtree, raw API) must refetch when the parent's
//     descendant_counts.children changes. No other skip exists.
//   - A dismissal inside a held range shortens the listing; nothing shifts.
func ChildOrder(a, b Session) bool { return childOrder(a, b) }

func childOrder(a, b Session) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	return a.ID > b.ID
}

func encodeCursor(s Session) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s.CreatedAt + "|" + s.ID))
}

// DecodeCursor returns the keyset a cursor names. ok is false for garbage.
func DecodeCursor(cursor string) (createdAt, id string, ok bool) {
	if cursor == "" {
		return "", "", true
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", false
	}
	createdAt, id, found := strings.Cut(string(raw), "|")
	if !found || id == "" {
		return "", "", false
	}
	return createdAt, id, true
}

// SortedChildren returns id's direct children (or, with descendants, its
// whole subtree) in listing order. ok is false when id is not in the payload.
// Every page and every scope window of one parent is cut from this one list,
// so callers that serve many of them per epoch should memoize it (the memo in
// cmd/gmuxd does).
func SortedChildren(rows []Session, idx *FamilyIndex, id string, descendants bool) ([]Session, bool) {
	if _, present := idx.byID[id]; !present {
		return nil, false
	}
	var ids []string
	if descendants {
		queue := append([]string(nil), idx.children[id]...)
		seen := map[string]bool{id: true}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if seen[cur] {
				continue
			}
			seen[cur] = true
			ids = append(ids, cur)
			queue = append(queue, idx.children[cur]...)
		}
	} else {
		ids = idx.children[id]
	}
	all := make([]Session, 0, len(ids))
	for _, kid := range ids {
		all = append(all, rows[idx.byID[kid]])
	}
	sort.Slice(all, func(i, j int) bool { return childOrder(all[i], all[j]) })
	return all, true
}

// cursorStart is the index of the first row strictly AFTER the cursor's
// position in a sorted listing (0 for an empty cursor).
func cursorStart(all []Session, cursor string) int {
	cAt, cID, ok := DecodeCursor(cursor)
	if !ok || cursor == "" {
		return 0
	}
	anchor := Session{CreatedAt: cAt, ID: cID}
	return sort.Search(len(all), func(i int) bool { return childOrder(anchor, all[i]) })
}

// Window is a scope's VIEWPORT (round-2 D2): the rows of a sorted listing
// from the head up to and including the cursor's position — exactly the
// pages a client that echoed next_cursor holds — capped at max rows. An
// empty cursor means "everything I have", still capped. Rows past the cap
// are pull-only for that client. A spawn (newer than the head) is inside
// every window; a row moving into the window by reparent is inside it; a row
// older than the bound is outside and is reported only through the total.
func Window(all []Session, cursor string, max int) []Session {
	if max < 1 {
		max = 1
	}
	end := len(all)
	if cursor != "" {
		end = cursorStart(all, cursor)
	}
	if end > max {
		end = max
	}
	return all[:end]
}

// ListChildren cuts one page of id's children out of an annotated payload.
// limit is clamped to [1, 200]. ok is false when id is not in the payload.
func ListChildren(rows []Session, idx *FamilyIndex, id string, descendants bool, cursor string, limit int) (ChildrenPage, bool) {
	all, ok := SortedChildren(rows, idx, id, descendants)
	if !ok {
		return ChildrenPage{}, false
	}
	return PageOf(all, id, descendants, cursor, limit), true
}

// PageOf cuts one page out of an already sorted listing.
func PageOf(all []Session, id string, descendants bool, cursor string, limit int) ChildrenPage {
	if limit < 1 || limit > 200 {
		if limit > 200 {
			limit = 200
		} else {
			limit = 100
		}
	}
	page := ChildrenPage{SessionID: id, Rows: []Session{}, Total: len(all), Descendants: descendants}
	start := cursorStart(all, cursor)
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	if start < end {
		page.Rows = all[start:end]
	}
	if end < len(all) {
		page.NextCursor = encodeCursor(all[end-1])
	}
	return page
}

// Ancestors returns id's family spine, root first, nearest parent last
// (empty for a root). Cut at the first repeated id.
func Ancestors(rows []Session, idx *FamilyIndex, id string) []Session {
	var reverse []Session
	seen := map[string]bool{id: true}
	cur := id
	for {
		parent, ok := idx.parentOf[cur]
		if !ok || seen[parent] {
			break
		}
		seen[parent] = true
		reverse = append(reverse, rows[idx.byID[parent]])
		cur = parent
	}
	for i, j := 0, len(reverse)-1; i < j; i, j = i+1, j-1 {
		reverse[i], reverse[j] = reverse[j], reverse[i]
	}
	return reverse
}
