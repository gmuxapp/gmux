package sessioncoord

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

const adNow = centralstore.UnixMillis(100 * 24 * 60 * 60 * 1000) // day 100

func adDay(d int) *centralstore.UnixMillis {
	v := centralstore.UnixMillis(int64(d) * 24 * 60 * 60 * 1000)
	return &v
}

// autoDismissFixture builds, in a real store:
//
//	root (dead, day 10)                     ← old, but a descendant stays visible
//	├── oldkid (dead, day 20)               ← eligible
//	├── unreadkid (dead, day 20, unread)    ← spared unless IncludeUnread
//	└── freshkid (dead, day 98)             ← recent
//	live (no exit fact, registry live)      ← never
//	├── livekid (dead, day 5)               ← eligible even under a live root
//	solo (dead, day 30)                     ← eligible
//	oldparent (dead, day 30)                ← eligible, and its subtree is closed
//	└── oldgrandkid (dead, day 40)          ← eligible
func autoDismissFixture(t *testing.T) (*Coordinator, *centralstore.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	parent := func(id string) *centralstore.SessionID { p := centralstore.SessionID(id); return &p }
	rows := []centralstore.NewSession{
		{ID: "root", Adapter: "shell", CreatedAt: 1, ExitedAt: adDay(10), LastActivityAt: adDay(10)},
		{ID: "oldkid", Adapter: "shell", CreatedAt: 2, ExitedAt: adDay(20), LastActivityAt: adDay(20), ParentSessionID: parent("root")},
		{ID: "unreadkid", Adapter: "shell", CreatedAt: 3, ExitedAt: adDay(20), LastActivityAt: adDay(20), Unread: true, UnreadToken: "t", ParentSessionID: parent("root")},
		{ID: "freshkid", Adapter: "shell", CreatedAt: 4, ExitedAt: adDay(98), LastActivityAt: adDay(98), ParentSessionID: parent("root")},
		{ID: "live", Adapter: "shell", CreatedAt: 5},
		{ID: "livekid", Adapter: "shell", CreatedAt: 6, ExitedAt: adDay(5), LastActivityAt: adDay(5), ParentSessionID: parent("live")},
		{ID: "solo", Adapter: "shell", CreatedAt: 7, ExitedAt: adDay(30), LastActivityAt: adDay(30)},
		{ID: "oldparent", Adapter: "shell", CreatedAt: 8, ExitedAt: adDay(30), LastActivityAt: adDay(30)},
		{ID: "oldgrandkid", Adapter: "shell", CreatedAt: 9, ExitedAt: adDay(40), LastActivityAt: adDay(40), ParentSessionID: parent("oldparent")},
	}
	for _, row := range rows {
		if _, _, err := store.InsertSession(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	coord := New(nil, nil, store, &fakeDirtySink{}, nil, WithClock(func() centralstore.UnixMillis { return adNow }))
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: "live", Generation: 1}, dead: make(chan struct{})})
	return coord, store
}

func visibleIDs(t *testing.T, store *centralstore.Store) []centralstore.SessionID {
	t.Helper()
	rows, err := store.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []centralstore.SessionID
	for _, r := range rows {
		if r.DismissedAt == nil {
			out = append(out, r.ID)
		}
	}
	return out
}

func TestAutoDismissKeepsUnreadAliveAndAncestorsOfVisibleRows(t *testing.T) {
	coord, store := autoDismissFixture(t)
	policy := AutoDismissPolicy{MaxIdle: 30 * 24 * time.Hour}

	report, err := coord.AutoDismiss(context.Background(), policy)
	if err != nil {
		t.Fatal(err)
	}
	want := []centralstore.SessionID{"livekid", "oldgrandkid", "oldkid", "oldparent", "solo"}
	if !reflect.DeepEqual(report.Candidates, want) || !reflect.DeepEqual(report.Dismissed, want) {
		t.Fatalf("candidates=%v dismissed=%v want %v", report.Candidates, report.Dismissed, want)
	}
	if report.KeptUnread != 1 || report.KeptAncestors != 1 || report.Visible != 9 {
		t.Fatalf("report=%+v", report)
	}
	got := visibleIDs(t, store)
	wantVisible := []centralstore.SessionID{"freshkid", "live", "root", "unreadkid"}
	if !reflect.DeepEqual(got, wantVisible) {
		t.Fatalf("visible=%v want %v", got, wantVisible)
	}
	// Hidden, not forgotten: the rows are still there with their facts.
	row, ok, err := store.Session(context.Background(), "oldkid")
	if err != nil || !ok || row.DismissedAt == nil || *row.DismissedAt != adNow || row.ExitedAt == nil || row.ParentSessionID == nil {
		t.Fatalf("dismissed row lost facts: ok=%v err=%v row=%+v", ok, err, row)
	}

	// Idempotent: a second pass finds nothing.
	again, err := coord.AutoDismiss(context.Background(), policy)
	if err != nil || len(again.Candidates) != 0 || len(again.Dismissed) != 0 {
		t.Fatalf("second pass: %+v err=%v", again, err)
	}
}

func TestAutoDismissIncludeUnreadClosesTheFamily(t *testing.T) {
	coord, store := autoDismissFixture(t)
	report, err := coord.AutoDismiss(context.Background(), AutoDismissPolicy{MaxIdle: 30 * 24 * time.Hour, IncludeUnread: true})
	if err != nil {
		t.Fatal(err)
	}
	// unreadkid now qualifies; root still does not (freshkid is recent).
	want := []centralstore.SessionID{"livekid", "oldgrandkid", "oldkid", "oldparent", "solo", "unreadkid"}
	if !reflect.DeepEqual(report.Dismissed, want) || report.KeptUnread != 0 || report.KeptAncestors != 1 {
		t.Fatalf("report=%+v", report)
	}
	if got := visibleIDs(t, store); !reflect.DeepEqual(got, []centralstore.SessionID{"freshkid", "live", "root"}) {
		t.Fatalf("visible=%v", got)
	}
}

func TestAutoDismissDryRunAndDisabledCommitNothing(t *testing.T) {
	coord, store := autoDismissFixture(t)
	before := visibleIDs(t, store)

	report, err := coord.AutoDismiss(context.Background(), AutoDismissPolicy{MaxIdle: 30 * 24 * time.Hour, DryRun: true})
	if err != nil || len(report.Candidates) != 5 || len(report.Dismissed) != 0 {
		t.Fatalf("dry run: %+v err=%v", report, err)
	}
	if got := visibleIDs(t, store); !reflect.DeepEqual(got, before) {
		t.Fatalf("dry run mutated the store: %v", got)
	}
	report, err = coord.AutoDismiss(context.Background(), AutoDismissPolicy{})
	if err != nil || len(report.Candidates) != 0 || report.Visible != 0 {
		t.Fatalf("disabled policy did work: %+v err=%v", report, err)
	}
}

func TestAutoDismissNeverHidesExitlessRowsDuringConvergence(t *testing.T) {
	coord, store := autoDismissFixture(t)
	// Reopen the convergence window: `live` has no exit fact and no runner
	// installed yet; it must survive regardless of its age.
	coord.registry = NewRegistry()
	if err := coord.BeginConvergence(context.Background()); err != nil {
		t.Fatal(err)
	}
	report, err := coord.AutoDismiss(context.Background(), AutoDismissPolicy{MaxIdle: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range report.Dismissed {
		if id == "live" {
			t.Fatalf("exit-less row hidden: %v", report.Dismissed)
		}
	}
	row, _, _ := store.Session(context.Background(), "live")
	if row.DismissedAt != nil {
		t.Fatal("live row dismissed")
	}
}
