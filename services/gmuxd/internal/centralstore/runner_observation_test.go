package centralstore

import (
	"context"
	"errors"
	"testing"
)

func TestApplyRunnerObservationAdvancesVersionActivityAndRejectsStale(t *testing.T) {
	s := openKernelStore(t)
	row, _, err := s.RegisterRunner(context.Background(), registration("event", "shell", "/one", true, 10))
	if err != nil {
		t.Fatal(err)
	}
	unread := true
	result, err := s.ApplyRunnerObservation(context.Background(), RunnerObservation{
		ID: row.ID, ObservedVersion: row.Version, ObservedAt: 20,
		Facts: RunnerFacts{Unread: &unread},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.SessionVersion != row.Version+1 || !result.SessionsDirty || result.WorldDirty {
		t.Fatalf("result=%+v", result)
	}
	raw, err := s.queries.GetSession(context.Background(), string(row.ID))
	if err != nil {
		t.Fatal(err)
	}
	got, err := sessionFromDB(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastActivityAt == nil || *got.LastActivityAt != 20 {
		t.Fatalf("activity=%v", got.LastActivityAt)
	}
	if _, err = s.ApplyRunnerObservation(context.Background(), RunnerObservation{ID: row.ID, ObservedVersion: row.Version, ObservedAt: 21}); !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("stale err=%v", err)
	}
}

func TestApplyRunnerObservationRebindClearsConversationMetadata(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	refA, title, subtitle, slug, shell := "A", "Title A", "subtitle A", "title-a", "terminal"
	reg := registration("rebind-event", "pi", "/one", true, 10)
	reg.Facts.ConversationRef = &refA
	reg.Facts.AdapterTitle = &title
	reg.Facts.Subtitle = &subtitle
	reg.Facts.Slug = &slug
	reg.Facts.ShellTitle = &shell
	row, _, err := s.RegisterRunner(ctx, reg)
	if err != nil {
		t.Fatal(err)
	}

	refB := "B"
	result, err := s.ApplyRunnerObservation(ctx, RunnerObservation{
		ID: row.ID, ObservedVersion: row.Version, ObservedAt: 20,
		Facts: RunnerFacts{ConversationRef: &refB, ResetStatus: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Session(ctx, row.ID)
	if err != nil || !ok {
		t.Fatalf("Session: ok=%v err=%v", ok, err)
	}
	if got.ConversationRef != refB || got.AdapterTitle != "" || got.Subtitle != "" || got.Slug != "" || got.SlugBase != "" {
		t.Fatalf("live rebind retained old conversation metadata: %#v", got)
	}
	if got.ShellTitle != shell {
		t.Fatalf("conversation rebind cleared generation-local shell title: %#v", got)
	}
	if !result.Changed || !result.SessionsDirty {
		t.Fatalf("result=%+v", result)
	}
}

func TestApplyRunnerObservationNoopDoesNotDirty(t *testing.T) {
	s := openKernelStore(t)
	row, _, err := s.RegisterRunner(context.Background(), registration("noop-event", "shell", "/one", true, 10))
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplyRunnerObservation(context.Background(), RunnerObservation{ID: row.ID, ObservedVersion: row.Version, ObservedAt: 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.SessionsDirty || result.SessionVersion != row.Version {
		t.Fatalf("result=%+v", result)
	}
}
