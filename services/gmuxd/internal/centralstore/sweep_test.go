package centralstore

import (
	"context"
	"testing"
)

func mustSession(t *testing.T, s *Store, id SessionID) Session {
	t.Helper()
	v, ok, err := s.Session(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("session %s: ok=%v err=%v", id, ok, err)
	}
	return v
}

func TestSweepDeadSessionsMarksUnclaimedRowsDead(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)

	live := registration("gone", "shell", "/one", true, 10)
	live.Facts.Working = ptr(true)
	live.Facts.Unread = ptr(true)
	before, _, err := s.RegisterRunner(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	if before.ExitedAt != nil || before.LastActivityAt == nil {
		t.Fatalf("precondition: %#v", before)
	}

	result, err := s.SweepDeadSessions(ctx, []SessionID{"gone"}, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.SessionsDirty || result.WorldDirty {
		t.Fatalf("result=%#v", result)
	}
	after := mustSession(t, s, "gone")
	if after.ExitedAt == nil || *after.ExitedAt != 500 {
		t.Fatalf("synthesized exit=%#v", after.ExitedAt)
	}
	if after.ExitCode != nil {
		t.Fatalf("exit code must stay unknown, got %#v", after.ExitCode)
	}
	// Turn state at death is the wait verdict and is preserved.
	if !after.Working || !after.Unread {
		t.Fatalf("turn state lost: %#v", after)
	}
	// Output-only last_output_at semantics: a synthesized death does not
	// bump activity — the row keeps the stamp from its last unread
	// transition (10) so the feed doesn't reshuffle on sweeps.
	if after.LastActivityAt == nil || *after.LastActivityAt != 10 {
		t.Fatalf("sweep must not bump activity: %#v", after.LastActivityAt)
	}
	if after.Version != before.Version+1 {
		t.Fatalf("version %d, want %d", after.Version, before.Version+1)
	}
}

// TestSweepDeadSessionsSweepsRowsWhoseVersionChurnedWithoutExit is the
// regression for the dropped row-version fence: version churn during the
// window does not resolve liveness, so it must not shield a row from the
// one-shot sweep.
func TestSweepDeadSessionsSweepsRowsWhoseVersionChurnedWithoutExit(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)

	// Flavor 1: a dead-row acknowledgement bumps the version but never sets
	// an exit timestamp.
	acked := registration("acked", "shell", "/one", true, 10)
	acked.Facts.Unread = ptr(true)
	ackRow, _, err := s.RegisterRunner(ctx, acked)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcknowledgeDeadSession(ctx, "acked", ackRow.Version); err != nil {
		t.Fatal(err)
	}

	// Flavor 2: a runner re-registers with changed facts (version bump),
	// then its stream drops without ever delivering exit facts.
	if _, _, err = s.RegisterRunner(ctx, registration("dropped", "shell", "/one", true, 10)); err != nil {
		t.Fatal(err)
	}
	rereg := registration("dropped", "shell", "/one/deeper", true, 20)
	rereg.Facts.Working = ptr(true)
	if _, _, err = s.RegisterRunner(ctx, rereg); err != nil {
		t.Fatal(err)
	}

	result, err := s.SweepDeadSessions(ctx, []SessionID{"acked", "dropped"}, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.SessionsDirty {
		t.Fatalf("result=%#v", result)
	}
	for _, id := range []SessionID{"acked", "dropped"} {
		after := mustSession(t, s, id)
		if after.ExitedAt == nil || *after.ExitedAt != 500 {
			t.Fatalf("version churn shielded %s from the sweep: %#v", id, after)
		}
	}
}

func TestSweepDeadSessionsSkipsExitedAndMissingRows(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)

	dead := registration("dead", "shell", "/one", false, 10)
	dead.Facts.ExitedAt = NullablePatch[UnixMillis]{Set: ptr(UnixMillis(11))}
	dead.Facts.ExitCode = NullablePatch[int]{Set: ptr(0)}
	if _, _, err := s.RegisterRunner(ctx, dead); err != nil {
		t.Fatal(err)
	}

	result, err := s.SweepDeadSessions(ctx, []SessionID{"dead", "never-existed"}, 500)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.SessionsDirty {
		t.Fatalf("result=%#v", result)
	}
	after := mustSession(t, s, "dead")
	if *after.ExitedAt != 11 || after.ExitCode == nil || *after.ExitCode != 0 {
		t.Fatalf("recorded exit facts overwritten: %#v", after)
	}
}

func TestSweepDeadSessionsSweepsDismissedRowWithoutTouchingDismissal(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)

	if _, _, err := s.RegisterRunner(ctx, registration("hidden", "shell", "/one", true, 10)); err != nil {
		t.Fatal(err)
	}
	// No dismissal domain operation exists in this slice; set the column
	// directly (a dismissed row also has no placement).
	if _, err := s.database.ExecContext(ctx,
		`UPDATE local_sessions SET dismissed_at_ms = 40 WHERE id = 'hidden'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.database.ExecContext(ctx,
		`DELETE FROM project_placements WHERE local_session_id = 'hidden'`); err != nil {
		t.Fatal(err)
	}

	result, err := s.SweepDeadSessions(ctx, []SessionID{"hidden"}, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.SessionsDirty {
		t.Fatalf("result=%#v", result)
	}
	after := mustSession(t, s, "hidden")
	if after.ExitedAt == nil || *after.ExitedAt != 500 {
		t.Fatalf("dismissed row not swept: %#v", after)
	}
	if after.DismissedAt == nil || *after.DismissedAt != 40 {
		t.Fatalf("sweep must not touch dismissal: %#v", after.DismissedAt)
	}
}

func TestSweepDeadSessionsActivityNeverMovesBackwards(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)

	live := registration("busy", "shell", "/one", true, 10)
	live.Facts.Unread = ptr(true)
	live.ObservedAt = 900 // activity bumped to 900 by the unread transition
	row, _, err := s.RegisterRunner(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastActivityAt == nil || *row.LastActivityAt != 900 {
		t.Fatalf("precondition activity=%#v", row.LastActivityAt)
	}

	result, err := s.SweepDeadSessions(ctx, []SessionID{"busy"}, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatalf("result=%#v", result)
	}
	after := mustSession(t, s, "busy")
	if *after.ExitedAt != 500 || after.LastActivityAt == nil || *after.LastActivityAt != 900 {
		t.Fatalf("activity moved backwards: %#v", after)
	}
}

func TestSweepDeadSessionsOneTransactionManyRows(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)

	var candidates []SessionID
	for _, id := range []string{"a", "b", "c"} {
		row, _, err := s.RegisterRunner(ctx, registration(id, "shell", "/one", true, 10))
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, row.ID)
	}
	result, err := s.SweepDeadSessions(ctx, candidates, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.SessionsDirty {
		t.Fatalf("result=%#v", result)
	}
	for _, id := range []string{"a", "b", "c"} {
		if after := mustSession(t, s, SessionID(id)); after.ExitedAt == nil || *after.ExitedAt != 500 {
			t.Fatalf("row %s not swept: %#v", id, after)
		}
	}
}

func TestSweepDeadSessionsValidation(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)

	if _, err := s.SweepDeadSessions(ctx, []SessionID{"x"}, -1); err == nil {
		t.Fatal("negative timestamp must be rejected")
	}
	if _, err := s.SweepDeadSessions(ctx, []SessionID{""}, 1); err == nil {
		t.Fatal("empty id must be rejected")
	}
	result, err := s.SweepDeadSessions(ctx, nil, 1)
	if err != nil || result.Changed || result.SessionsDirty {
		t.Fatalf("empty sweep must be a silent no-op: %#v err=%v", result, err)
	}
}
