package centralstore

import (
	"context"
	"errors"
	"testing"
)

func TestSuppressedRunnerResultCommitsTokenRecencyAndPreservesError(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	row, _, err := s.RegisterRunner(ctx, registration("supervised", "shell", "/one", true, 10))
	if err != nil {
		t.Fatal(err)
	}
	unread, hasError, token, exitCode, exitedAt := true, true, "fresh-result", 0, UnixMillis(20)
	result, err := s.ApplyRunnerObservation(ctx, RunnerObservation{
		ID: row.ID, ObservedVersion: row.Version, ObservedAt: 20, SuppressUnread: true,
		Facts: RunnerFacts{
			Unread: &unread, UnreadToken: &token, Error: &hasError,
			ExitCode: NullablePatch[int]{Set: &exitCode}, ExitedAt: NullablePatch[UnixMillis]{Set: &exitedAt},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("suppressed completion did not mutate row")
	}
	got, ok, err := s.Session(ctx, row.ID)
	if err != nil || !ok {
		t.Fatalf("Session: ok=%v err=%v", ok, err)
	}
	if got.Unread || got.UnreadToken != token || !got.Error {
		t.Fatalf("suppressed completion erased result facts: %+v", got)
	}
	if got.LastActivityAt == nil || *got.LastActivityAt != 20 {
		t.Fatalf("completion recency=%v, want 20", got.LastActivityAt)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 || got.ExitedAt == nil || *got.ExitedAt != 20 {
		t.Fatalf("exit facts not committed atomically: %+v", got)
	}
}

func TestSuppressedRunnerResultRequiresFreshGeneration(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	row, _, err := s.RegisterRunner(ctx, registration("invalid-suppression", "shell", "/one", true, 10))
	if err != nil {
		t.Fatal(err)
	}
	unread := true
	if _, err := s.ApplyRunnerObservation(ctx, RunnerObservation{ID: row.ID, ObservedVersion: row.Version, ObservedAt: 20, SuppressUnread: true, Facts: RunnerFacts{Unread: &unread}}); err == nil {
		t.Fatal("suppression without a fresh token succeeded")
	}
	got, _, _ := s.Session(ctx, row.ID)
	if got.Version != row.Version || got.Unread {
		t.Fatalf("invalid suppression changed row: %+v", got)
	}
}

func TestSuppressedRunnerResultCannotClearOlderUnread(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	row, _, err := s.RegisterRunner(ctx, registration("older-unread", "shell", "/one", true, 10))
	if err != nil {
		t.Fatal(err)
	}
	olderUnread, olderToken := true, "older-result"
	result, err := s.ApplyRunnerObservation(ctx, RunnerObservation{ID: row.ID, ObservedVersion: row.Version, ObservedAt: 15, Facts: RunnerFacts{Unread: &olderUnread, UnreadToken: &olderToken}})
	if err != nil {
		t.Fatal(err)
	}
	newToken := "new-result"
	if _, err := s.ApplyRunnerObservation(ctx, RunnerObservation{ID: row.ID, ObservedVersion: result.SessionVersion, ObservedAt: 20, SuppressUnread: true, Facts: RunnerFacts{Unread: &olderUnread, UnreadToken: &newToken}}); !errors.Is(err, ErrSuppressionWouldClearUnread) {
		t.Fatalf("suppression error=%v", err)
	}
	got, _, _ := s.Session(ctx, row.ID)
	if !got.Unread || got.UnreadToken != olderToken || got.Version != result.SessionVersion {
		t.Fatalf("older attention changed: %+v", got)
	}
}

func TestSuppressedFastDeadRegistrationIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	reg := registration("fast-dead", "shell", "/one", false, 20)
	unread, token, code, exited := true, "fast-result", 0, UnixMillis(20)
	reg.Facts.Unread, reg.Facts.UnreadToken = &unread, &token
	reg.Facts.ExitCode, reg.Facts.ExitedAt = NullablePatch[int]{Set: &code}, NullablePatch[UnixMillis]{Set: &exited}
	reg.SuppressUnread = true
	row, _, err := s.RegisterRunner(ctx, reg)
	if err != nil {
		t.Fatal(err)
	}
	if row.Unread || row.UnreadToken != token || row.ExitCode == nil || *row.ExitCode != 0 || row.LastActivityAt == nil || *row.LastActivityAt != 20 {
		t.Fatalf("suppressed fast-dead registration lost facts: %+v", row)
	}
}

func TestSuppressedFastDeadRegistrationCannotClearExistingUnread(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	row, _, err := s.RegisterRunner(ctx, registration("fast-replace", "shell", "/one", true, 10))
	if err != nil {
		t.Fatal(err)
	}
	olderUnread, olderToken := true, "older"
	result, err := s.ApplyRunnerObservation(ctx, RunnerObservation{ID: row.ID, ObservedVersion: row.Version, ObservedAt: 15, Facts: RunnerFacts{Unread: &olderUnread, UnreadToken: &olderToken}})
	if err != nil {
		t.Fatal(err)
	}
	code, exited, fresh := 0, UnixMillis(20), "fresh"
	reg := registration(string(row.ID), "shell", "/one", false, 20)
	reg.NewGeneration, reg.SuppressUnread = true, true
	reg.Facts.Unread, reg.Facts.UnreadToken = &olderUnread, &fresh
	reg.Facts.ExitCode, reg.Facts.ExitedAt = NullablePatch[int]{Set: &code}, NullablePatch[UnixMillis]{Set: &exited}
	if _, _, err := s.RegisterRunner(ctx, reg); !errors.Is(err, ErrSuppressionWouldClearUnread) {
		t.Fatalf("registration suppression error=%v", err)
	}
	got, _, _ := s.Session(ctx, row.ID)
	if !got.Unread || got.UnreadToken != olderToken || got.Version != result.SessionVersion {
		t.Fatalf("older result changed: %+v", got)
	}
}
