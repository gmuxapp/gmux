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
	working := true
	result, err := s.ApplyRunnerObservation(context.Background(), RunnerObservation{
		ID: row.ID, ObservedVersion: row.Version, ObservedAt: 20,
		Facts: RunnerFacts{Working: &working},
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
