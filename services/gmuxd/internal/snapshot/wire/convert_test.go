package wire

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

func ms(v int64) *centralstore.UnixMillis {
	m := centralstore.UnixMillis(v)
	return &m
}

func localRow(id string, alive bool, mut ...func(*central.SessionRow)) central.SessionRow {
	row := central.SessionRow{SessionView: centralstore.SessionView{Session: centralstore.Session{
		ID: centralstore.SessionID(id), Adapter: "shell", Command: []string{"bash"},
		CreatedAt: 1700000000000, StatusReported: true,
	}}}
	if alive {
		row.Alive = true
		row.Runtime = &central.RuntimeFacts{PID: 42, Endpoint: "/tmp/" + id + ".sock", RunnerVersion: "1.2.3", BinaryHash: "abc"}
	} else {
		row.Resumable = true
	}
	for _, m := range mut {
		m(&row)
	}
	return row
}

func place(row *central.SessionRow, slug, scope string, pos int) {
	row.SessionView.Placement = &centralstore.SessionPlacement{ProjectSlug: slug, SiblingScope: scope, Position: pos}
}

// TestTitlePrecedence pins the resolveTitle chain against the production
// precedence (store.resolveTitle): adapter title > shell title >
// CommandTitler(command) > adapter name.
func TestTitlePrecedence(t *testing.T) {
	conv := &Converter{Titlers: map[string]func([]string) string{
		"codex": func(cmd []string) string { return "titled:" + cmd[0] },
	}}
	cases := []struct {
		name                              string
		adapterTitle, shellTitle, adapter string
		command                           []string
		want                              string
	}{
		{"adapter title wins", "A", "S", "codex", []string{"codex"}, "A"},
		{"shell title next", "", "S", "codex", []string{"codex"}, "S"},
		{"command titler next", "", "", "codex", []string{"codex", "resume", "x"}, "titled:codex"},
		{"titler skipped without command", "", "", "codex", nil, "codex"},
		{"no titler falls to adapter name", "", "", "claude", []string{"claude"}, "claude"},
		{"empty titles with command, no titler", "", "", "shell", []string{"bash"}, "shell"},
	}
	for _, tc := range cases {
		s := centralstore.Session{AdapterTitle: tc.adapterTitle, ShellTitle: tc.shellTitle, Adapter: tc.adapter, Command: tc.command}
		if got := conv.resolveTitle(s); got != tc.want {
			t.Errorf("%s: title=%q want %q", tc.name, got, tc.want)
		}
	}
}

// TestSessionConversionOverlays: runtime facts ride only alive rows;
// timestamps convert Unix-ms → RFC3339 seconds; a reported status is
// concrete on the wire.
func TestSessionConversionOverlays(t *testing.T) {
	conv := &Converter{}
	alive := localRow("sess-a", true, func(r *central.SessionRow) {
		r.Session.StartedAt = ms(1700000001500) // sub-second component truncated
		r.Session.Working = true
		cols, rows := uint16(80), uint16(24)
		r.Session.TerminalCols, r.Session.TerminalRows = &cols, &rows
	})
	got := conv.session(alive)
	if !got.Alive || got.Pid != 42 || got.SocketPath != "/tmp/sess-a.sock" || got.RunnerVersion != "1.2.3" || got.BinaryHash != "abc" {
		t.Fatalf("runtime overlay lost: %+v", got)
	}
	if got.CreatedAt != "2023-11-14T22:13:20Z" || got.StartedAt != "2023-11-14T22:13:21Z" {
		t.Fatalf("timestamps: created=%q started=%q", got.CreatedAt, got.StartedAt)
	}
	if got.Status == nil || !got.Status.Working || got.Status.Error {
		t.Fatalf("status: %+v", got.Status)
	}
	if got.TerminalCols != 80 || got.TerminalRows != 24 {
		t.Fatalf("terminal dims: %+v", got)
	}
	if got.Resumable {
		t.Fatal("alive row must not be resumable")
	}

	// Never-reported status emits null (production Status-pointer parity;
	// gmux wait derives died-vs-idle from this — not an accepted diff).
	unreported := localRow("sess-n", true, func(r *central.SessionRow) { r.Session.StatusReported = false })
	if got := conv.session(unreported); got.Status != nil {
		t.Fatalf("never-reported status must be null on the wire: %+v", got.Status)
	}

	dead := localRow("sess-d", false, func(r *central.SessionRow) {
		r.Session.ExitedAt = ms(1700000002000)
		code := 1
		r.Session.ExitCode = &code
	})
	gd := conv.session(dead)
	if gd.Alive || gd.Pid != 0 || gd.SocketPath != "" || gd.RunnerVersion != "" || gd.BinaryHash != "" {
		t.Fatalf("dead row leaked runtime fields: %+v", gd)
	}
	if !gd.Resumable || gd.ExitedAt != "2023-11-14T22:13:22Z" || gd.ExitCode == nil || *gd.ExitCode != 1 {
		t.Fatalf("dead row: %+v", gd)
	}
}

// TestResumeCommandRewrite: dead rows show the resume form derived from
// (adapter, ref); the durable command is untouched; a nil resolver or nil
// resolution keeps the launch command; resumability follows the rewritten
// command and the composer's verdict narrowing.
func TestResumeCommandRewrite(t *testing.T) {
	resolver := func(adapter, ref string) []string {
		if ref == "known-ref" {
			return []string{adapter, "resume", ref}
		}
		return nil
	}
	conv := &Converter{ResumeCommand: resolver}

	dead := localRow("sess-d", false, func(r *central.SessionRow) { r.Session.ConversationRef = "known-ref" })
	if got := conv.session(dead); !reflect.DeepEqual(got.Command, []string{"shell", "resume", "known-ref"}) || !got.Resumable {
		t.Fatalf("rewrite: %+v", got)
	}
	if !reflect.DeepEqual(dead.Session.Command, []string{"bash"}) {
		t.Fatal("durable command mutated")
	}

	live := localRow("sess-a", true, func(r *central.SessionRow) { r.Session.ConversationRef = "known-ref" })
	if got := conv.session(live); !reflect.DeepEqual(got.Command, []string{"bash"}) {
		t.Fatalf("live row rewritten: %+v", got)
	}

	unresolved := localRow("sess-u", false, func(r *central.SessionRow) { r.Session.ConversationRef = "gone-ref" })
	if got := conv.session(unresolved); !reflect.DeepEqual(got.Command, []string{"bash"}) || !got.Resumable {
		t.Fatalf("nil resolution must keep launch command: %+v", got)
	}

	// Verdict narrowing (composer Resumable=false despite durable command)
	// wins over a successful rewrite.
	gone := localRow("sess-g", false, func(r *central.SessionRow) {
		r.Session.ConversationRef = "known-ref"
		r.Resumable = false
	})
	if got := conv.session(gone); got.Resumable {
		t.Fatalf("verdict-gone row stayed resumable: %+v", got)
	}

	// Empty durable command + a resolvable ref: resumable derives from the
	// rewritten command (production parity — Resumable follows the
	// post-rewrite command).
	empty := localRow("sess-e", false, func(r *central.SessionRow) {
		r.Session.Command = []string{}
		r.Session.ConversationRef = "known-ref"
		r.Resumable = false // composer: no durable command
	})
	if got := conv.session(empty); !got.Resumable || len(got.Command) != 3 {
		t.Fatalf("empty-command rewrite: %+v", got)
	}
}

func lpPlacement(peer, sess, slug, scope string, pos int) central.LocalPeerPlacementRow {
	return central.LocalPeerPlacementRow{LocalPeerPlacementView: centralstore.LocalPeerPlacementView{
		PeerKey: centralstore.PeerKey(peer), SessionID: sess, ProjectSlug: slug, SiblingScope: scope, Position: pos,
	}}
}

// TestFlattenInlinesChildBlocks is the FD-1 core: roots in root-scope
// order, each child block inlined after its parent (recursively), Local-peer
// children participating, renumbered 0..n-1 project-wide.
func TestFlattenInlinesChildBlocks(t *testing.T) {
	conv := &Converter{IsLocalPeer: func(n string) bool { return n == "box" }}
	rows := []central.SessionRow{
		localRow("root-b", true), localRow("root-a", true),
		localRow("child-a1", true), localRow("grand-a1", false),
	}
	place(&rows[0], "proj", "r", 1)
	place(&rows[1], "proj", "r", 0)
	place(&rows[2], "proj", "c:l:root-a", 0)
	place(&rows[3], "proj", "c:l:child-a1", 0)
	world := &central.ProjectsPayload{LocalPeerPlacements: []central.LocalPeerPlacementRow{
		lpPlacement("box", "cont-1", "proj", "c:l:root-a", 1),
	}}
	peerRows := []Session{{ID: "cont-1@box", Peer: "box", Adapter: "shell", Alive: true}}

	got := conv.Sessions(&central.SessionsPayload{Sessions: rows}, world, peerRows)
	idx := map[string]int{}
	slug := map[string]string{}
	for _, s := range got.Sessions {
		idx[s.ID] = s.ProjectIndex
		slug[s.ID] = s.ProjectSlug
	}
	want := map[string]int{"root-a": 0, "child-a1": 1, "grand-a1": 2, "cont-1@box": 3, "root-b": 4}
	for id, w := range want {
		if idx[id] != w {
			t.Fatalf("project_index: got %v want %v", idx, want)
		}
		if slug[id] != "proj" {
			t.Fatalf("project_slug for %s: %q", id, slug[id])
		}
	}
	// Deterministic ID sort on the wire.
	for i := 1; i < len(got.Sessions); i++ {
		if got.Sessions[i-1].ID >= got.Sessions[i].ID {
			t.Fatalf("sessions not ID-sorted: %v", got.Sessions)
		}
	}
}

// TestPeerRowStamping: network-peer rows pass through with origin stamps
// verbatim; unplaced Local-peer rows get cleared stamps (the parent
// disclaims them); the source slice is never mutated.
func TestPeerRowStamping(t *testing.T) {
	conv := &Converter{IsLocalPeer: func(n string) bool { return n == "box" }}
	src := []Session{
		{ID: "net-1@tower", Peer: "tower", Adapter: "shell", ProjectSlug: "origin-proj", ProjectIndex: 7},
		{ID: "cont-9@box", Peer: "box", Adapter: "shell", ProjectSlug: "stale-stamp", ProjectIndex: 3},
	}
	got := conv.Sessions(nil, &central.ProjectsPayload{}, src)
	for _, s := range got.Sessions {
		switch s.ID {
		case "net-1@tower":
			if s.ProjectSlug != "origin-proj" || s.ProjectIndex != 7 {
				t.Fatalf("network stamps not verbatim: %+v", s)
			}
		case "cont-9@box":
			if s.ProjectSlug != "" || s.ProjectIndex != 0 {
				t.Fatalf("unplaced Local-peer stamps not cleared: %+v", s)
			}
		}
	}
	if src[1].ProjectSlug != "stale-stamp" {
		t.Fatal("source slice mutated")
	}
}

// TestWorldConversion: FD-5 sessions[] rebuild in flatten order with
// namespaced Local-peer keys, reference items keyed by peer, FD-6 counts
// derived from rows, and the PeerSource health blob copied (not mutated).
func TestWorldConversion(t *testing.T) {
	conv := &Converter{IsLocalPeer: func(n string) bool { return n == "box" }}
	rows := []central.SessionRow{localRow("s-1", true), localRow("s-2", false)}
	place(&rows[0], "proj", "r", 0)
	place(&rows[1], "proj", "c:l:s-1", 0)
	health := &central.HealthInfo{Hostname: "hub"}
	world := &central.ProjectsPayload{
		Projects: centralstore.ProjectCatalog{
			{Kind: centralstore.ProjectEntryOwned, Slug: "proj", Rules: []centralstore.MatchRule{{Path: "/x", Exact: true}}},
			{Kind: centralstore.ProjectEntryReference, Slug: "remote", PeerKey: "tower", NodeID: "node-tower"},
		},
		LocalPeerPlacements: []central.LocalPeerPlacementRow{lpPlacement("box", "c-1", "proj", "r", 1)},
		Health:              health,
	}
	peerRows := []Session{
		{ID: "c-1@box", Peer: "box", Alive: true},
		{ID: "n-1@tower", Peer: "tower", Alive: false},
	}
	got := conv.World(&central.SessionsPayload{Sessions: rows}, world, peerRows)
	if len(got.Projects) != 2 {
		t.Fatalf("projects: %+v", got.Projects)
	}
	owned := got.Projects[0]
	if !reflect.DeepEqual(owned.Sessions, []string{"s-1", "s-2", "c-1@box"}) {
		t.Fatalf("FD-5 sessions[]: %v", owned.Sessions)
	}
	if !reflect.DeepEqual(owned.Match, []MatchRule{{Path: "/x", Exact: true}}) {
		t.Fatalf("match rules: %+v", owned.Match)
	}
	ref := got.Projects[1]
	if ref.Peer != "tower" || ref.NodeID != "node-tower" || ref.Sessions != nil || ref.Match != nil {
		t.Fatalf("reference item: %+v", ref)
	}
	want := central.SessionCounts{LocalAlive: 1, RemoteAlive: 1, Dead: 2}
	if got.Health.Sessions != want {
		t.Fatalf("FD-6 counts: %+v", got.Health.Sessions)
	}
	if health.Sessions != (central.SessionCounts{}) {
		t.Fatal("PeerSource health blob mutated")
	}

	// Repeated conversion from the same PeerSource payload is stable and
	// still leaves the source blob untouched (tests review M-4: the
	// PeerSource is called repeatedly in production; state must not leak
	// across conversions).
	again := conv.World(&central.SessionsPayload{Sessions: rows}, world, peerRows)
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("second conversion diverged:\n%+v\n%+v", got, again)
	}
	if health.Sessions != (central.SessionCounts{}) {
		t.Fatal("PeerSource health blob mutated on second conversion")
	}
}

// TestFlattenStragglerGetsDeterministicTailIndex (tests review H-2): a
// placement whose scope references a parent outside the project's
// participant set (cross-payload-kind skew) must not vanish — it gets a
// deterministic index after the reachable tree, ordered scope-then-pos.
func TestFlattenStragglerGetsDeterministicTailIndex(t *testing.T) {
	conv := &Converter{}
	rows := []central.SessionRow{
		localRow("root-a", true), localRow("stray-b", true), localRow("stray-a", true),
	}
	place(&rows[0], "proj", "r", 0)
	place(&rows[1], "proj", "c:l:ghost-parent", 1)
	place(&rows[2], "proj", "c:l:ghost-parent", 0)
	got := conv.Sessions(&central.SessionsPayload{Sessions: rows}, &central.ProjectsPayload{}, nil)
	idx := map[string]int{}
	for _, s := range got.Sessions {
		if s.ProjectSlug != "proj" {
			t.Fatalf("straggler lost its project stamp: %+v", s)
		}
		idx[s.ID] = s.ProjectIndex
	}
	want := map[string]int{"root-a": 0, "stray-a": 1, "stray-b": 2}
	for id, w := range want {
		if idx[id] != w {
			t.Fatalf("straggler tail indices: got %v want %v", idx, want)
		}
	}
}

// TestFilterOwned pins the ?as=peer narrowing: own + Local-peer only.
func TestFilterOwned(t *testing.T) {
	p := SessionsPayload{Sessions: []Session{
		{ID: "local"}, {ID: "c@box", Peer: "box"}, {ID: "n@tower", Peer: "tower"},
	}}
	got := p.FilterOwned(func(n string) bool { return n == "box" })
	if len(got.Sessions) != 2 || got.Sessions[0].ID != "local" || got.Sessions[1].ID != "c@box" {
		t.Fatalf("filtered: %+v", got.Sessions)
	}
}

// TestWireSessionShapeMatchesProduction is the JSON-shape pin: a wire.Session
// and a production store.Session carrying the same values marshal to
// byte-identical JSON (both the fully-populated and the zero-ish shape).
func TestWireSessionShapeMatchesProduction(t *testing.T) {
	code := 3
	w := Session{
		ID: "s", Peer: "p", CreatedAt: "2023-11-14T22:13:20Z", Command: []string{"bash", "-l"},
		Cwd: "/x", Adapter: "shell", WorkspaceRoot: "/w", Remotes: map[string]string{"origin": "git@x"},
		ParentSessionID: "par", Alive: true, Pid: 9, ExitCode: &code,
		StartedAt: "2023-11-14T22:13:21Z", ExitedAt: "2023-11-14T22:13:22Z",
		Title: "T", Subtitle: "sub", Status: &Status{Working: true, Error: true}, Unread: true,
		Resumable: true, SocketPath: "/s.sock", TerminalCols: 80, TerminalRows: 24,
		Slug: "slug", ConversationRef: "/conv", RunnerVersion: "v", BinaryHash: "h",
		ProjectSlug: "proj", ProjectIndex: 2, LastActivityAt: "2023-11-14T22:13:23Z",
	}
	prod := store.Session{
		ID: "s", Peer: "p", CreatedAt: "2023-11-14T22:13:20Z", Command: []string{"bash", "-l"},
		Cwd: "/x", Adapter: "shell", WorkspaceRoot: "/w", Remotes: map[string]string{"origin": "git@x"},
		ParentSessionID: "par", Alive: true, Pid: 9, ExitCode: &code,
		StartedAt: "2023-11-14T22:13:21Z", ExitedAt: "2023-11-14T22:13:22Z",
		Title: "T", Subtitle: "sub", Status: &store.Status{Working: true, Error: true}, Unread: true,
		Resumable: true, SocketPath: "/s.sock", TerminalCols: 80, TerminalRows: 24,
		Slug: "slug", ConversationRef: "/conv", RunnerVersion: "v", BinaryHash: "h",
		ProjectSlug: "proj", ProjectIndex: 2, LastActivityAt: "2023-11-14T22:13:23Z",
	}
	wj, _ := json.Marshal(w)
	pj, _ := json.Marshal(prod)
	if string(wj) != string(pj) {
		t.Fatalf("populated shape diverged:\nwire: %s\nprod: %s", wj, pj)
	}

	wj, _ = json.Marshal(Session{ID: "s", Adapter: "shell", Status: &Status{}})
	pj, _ = json.Marshal(store.Session{ID: "s", Adapter: "shell", Status: &store.Status{}})
	if string(wj) != string(pj) {
		t.Fatalf("minimal shape diverged:\nwire: %s\nprod: %s", wj, pj)
	}
}

// TestCacheAppliesBatchesAndRecomposesSessionsOnWorldDirt: a projects-only
// batch re-emits snapshot.sessions with updated placement stamps
// (production parity: projects-update fired both coalescers); Current
// yields a matched pair for late subscribers.
func TestCacheAppliesBatchesAndRecomposesSessionsOnWorldDirt(t *testing.T) {
	conv := &Converter{}
	cache := NewCache(conv, nil)

	row := localRow("s-1", true)
	place(&row, "proj", "r", 1)
	sibling := localRow("s-0", true)
	place(&sibling, "proj", "r", 0)

	// A projects-only FIRST batch is withheld entirely (fable L-1): no
	// world frame with session-less owned projects, no sessions frame.
	f := cache.Apply(central.Batch{Projects: &central.ProjectsPayload{Projects: centralstore.ProjectCatalog{{Kind: centralstore.ProjectEntryOwned, Slug: "proj"}}}})
	if f.Sessions != nil || f.World != nil {
		t.Fatalf("projects-only first batch must be withheld: %+v", f)
	}
	if cur := cache.Current(); cur.Sessions != nil || cur.World != nil {
		t.Fatalf("Current before any sessions payload must be empty: %+v", cur)
	}

	f = cache.Apply(central.Batch{Sessions: &central.SessionsPayload{Sessions: []central.SessionRow{row, sibling}}})
	if f.Sessions == nil || f.World != nil {
		t.Fatalf("frames: %+v", f)
	}
	if idxOf(t, f.Sessions, "s-1") != 1 {
		t.Fatalf("initial index: %+v", f.Sessions)
	}

	// Reorder: world-dirty commit; the sessions frame must be re-emitted
	// with the new flat indices.
	row2, sib2 := row, sibling
	place(&row2, "proj", "r", 0)
	place(&sib2, "proj", "r", 1)
	f = cache.Apply(central.Batch{
		Sessions: &central.SessionsPayload{Sessions: []central.SessionRow{row2, sib2}},
		Projects: &central.ProjectsPayload{Projects: centralstore.ProjectCatalog{{Kind: centralstore.ProjectEntryOwned, Slug: "proj"}}},
	})
	if f.Sessions == nil || f.World == nil {
		t.Fatalf("matched pair expected: %+v", f)
	}
	if idxOf(t, f.Sessions, "s-1") != 0 {
		t.Fatalf("reordered index: %+v", f.Sessions)
	}

	cur := cache.Current()
	if cur.Sessions == nil || cur.World == nil || idxOf(t, cur.Sessions, "s-1") != 0 {
		t.Fatalf("Current: %+v", cur)
	}

	// Projects-only batch (e.g. peer status dirt): sessions frame still
	// recomposed from cache.
	f = cache.Apply(central.Batch{Projects: &central.ProjectsPayload{}})
	if f.Sessions == nil || f.World == nil {
		t.Fatalf("projects-only apply: %+v", f)
	}
}

func idxOf(t *testing.T, p *SessionsPayload, id string) int {
	t.Helper()
	for _, s := range p.Sessions {
		if s.ID == id {
			return s.ProjectIndex
		}
	}
	t.Fatalf("session %s not in payload %+v", id, p)
	return -1
}
