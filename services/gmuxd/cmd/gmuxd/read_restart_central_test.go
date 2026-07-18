package main

import (
	"context"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

func TestReadAcknowledgementDurableAcrossCloseReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := centralstore.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	unread := true
	exited := centralstore.UnixMillis(1)
	row, _, err := st.RegisterRunner(ctx, centralstore.RunnerRegistration{ID: "s", Adapter: "shell", Alive: false, CreatedAt: 1, ObservedAt: 1, Facts: centralstore.RunnerFacts{Unread: &unread, ExitedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &exited}}})
	if err != nil {
		t.Fatal(err)
	}
	coord := sessioncoord.New(nil, &bootstrapRunners{metas: map[string]sessioncoord.RunnerMeta{}, blocked: map[string]bool{}}, st, nil, nil)
	if err := coord.AcknowledgeDead(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	coord.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = centralstore.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, ok, err := st.Session(ctx, "s")
	if err != nil || !ok {
		t.Fatalf("session ok=%v err=%v", ok, err)
	}
	if got.Unread {
		t.Fatal("unread resurrected after reopen")
	}
}
