package centralstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func registration(id, adapter, cwd string, alive bool, at UnixMillis) RunnerRegistration {
	cmd := []string{"sh"}
	remotes := map[string]string{}
	return RunnerRegistration{
		ID: SessionID(id), Adapter: adapter, Alive: alive, CreatedAt: at, ObservedAt: at,
		Facts: RunnerFacts{CWD: &cwd, Command: &cmd, Remotes: &remotes},
	}
}

func registrationCatalog(t *testing.T, s *Store) ProjectCatalog {
	t.Helper()
	cat, _, err := s.ReplaceProjectCatalog(context.Background(), []ProjectEntrySpec{
		owned("one", "/one"), owned("two", "/two"),
		{Reference: &ProjectReference{PeerKey: "remote", Slug: "one"}},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func localPlacement(t *testing.T, s *Store, id SessionID) *placementRec {
	t.Helper()
	all, err := placements(context.Background(), s.queries)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		if p.local == string(id) {
			return p
		}
	}
	return nil
}

func TestRegisterRunnerNewLiveAndFastDead(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)
	live := registration("live", "shell", "/one/src", true, 10)
	live.Facts.Working = ptr(true)
	got, result, err := s.RegisterRunner(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.CreatedAt != 10 || got.ExitedAt != nil || !got.Working || !result.Changed || !result.SessionsDirty || !result.WorldDirty {
		t.Fatalf("live=%#v result=%#v", got, result)
	}
	if p := localPlacement(t, s, "live"); p == nil || p.project != int64(cat[0].ID) || p.pos != 0 {
		t.Fatalf("live placement=%#v", p)
	}

	dead := registration("dead", "shell", "/one", false, 20)
	dead.Facts.Working = ptr(false)
	dead.Facts.Unread = ptr(true)
	dead.Facts.Error = ptr(true)
	dead.Facts.ExitedAt = NullablePatch[UnixMillis]{Set: ptr(UnixMillis(21))}
	dead.Facts.ExitCode = NullablePatch[int]{Set: ptr(7)}
	got, result, err = s.RegisterRunner(ctx, dead)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitedAt == nil || *got.ExitedAt != 21 || got.ExitCode == nil || *got.ExitCode != 7 || !got.Unread || !got.Error || result.SessionVersion != 1 {
		t.Fatalf("dead=%#v result=%#v", got, result)
	}
	// A brand-new fast-dead row must not sort as "no activity": the collapsed
	// insert seeds activity from the observation time.
	if got.LastActivityAt == nil || *got.LastActivityAt != 20 {
		t.Fatalf("fast-dead activity=%#v", got.LastActivityAt)
	}
	if p := localPlacement(t, s, "dead"); p == nil || p.pos != 1 {
		t.Fatalf("fast-dead placement=%#v", p)
	}
}

func TestRegisterRunnerSameIDPreservesHistoryAndNoop(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)
	parent := SessionID("original-parent")
	first := registration("same", "shell", "/one", false, 10)
	first.LaunchParentID = &parent
	first.Facts.ExitedAt = NullablePatch[UnixMillis]{Set: ptr(UnixMillis(12))}
	first.Facts.ExitCode = NullablePatch[int]{Set: ptr(3)}
	got, _, err := s.RegisterRunner(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetPromotion(ctx, "same", true, nil); err != nil {
		t.Fatal(err)
	}
	beforePlacement := localPlacement(t, s, "same")
	beforePos, beforeProject := beforePlacement.pos, beforePlacement.project

	otherParent := SessionID("replacement-parent")
	resume := registration("same", "shell", "/one", true, 99)
	resume.LaunchParentID = &otherParent
	got, result, err := s.RegisterRunner(ctx, resume)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt != 10 || got.LaunchParentID == nil || *got.LaunchParentID != parent || !got.PromotedToRoot || got.ExitedAt != nil || got.ExitCode != nil {
		t.Fatalf("history not preserved/live exit not cleared: %#v", got)
	}
	if result.SessionVersion != 3 || !result.SessionsDirty || result.WorldDirty {
		t.Fatalf("resume result=%#v", result)
	}
	p := localPlacement(t, s, "same")
	if p.project != beforeProject || p.pos != beforePos || p.project != int64(cat[0].ID) {
		t.Fatalf("placement moved: %#v", p)
	}

	noop := registration("same", "shell", "/ignored-created", true, 100)
	noop.Facts = RunnerFacts{}
	_, result, err = s.RegisterRunner(ctx, noop)
	if err != nil || result.Changed || result.SessionsDirty || result.WorldDirty || result.SessionVersion != 3 {
		t.Fatalf("noop result=%#v err=%v", result, err)
	}

	mismatch := registration("same", "pi", "/one", true, 101)
	_, result, err = s.RegisterRunner(ctx, mismatch)
	if !errors.Is(err, ErrAdapterMismatch) || result.SessionVersion != 3 {
		t.Fatalf("mismatch result=%#v err=%v", result, err)
	}
}

func TestRegisterRunnerTriStateFactsAndActivityTransitions(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	started, exited := UnixMillis(2), UnixMillis(3)
	size := TerminalSize{Cols: 80, Rows: 24}
	reg := registration("facts", "shell", "/none", false, 1)
	reg.Facts.ConversationRef = ptr("conversation")
	reg.Facts.WorkspaceRoot = ptr("/workspace")
	reg.Facts.Slug = ptr("slug")
	reg.Facts.ShellTitle = ptr("shell")
	reg.Facts.AdapterTitle = ptr("adapter")
	reg.Facts.Subtitle = ptr("subtitle")
	reg.Facts.StartedAt = NullablePatch[UnixMillis]{Set: &started}
	reg.Facts.ExitedAt = NullablePatch[UnixMillis]{Set: &exited}
	reg.Facts.ExitCode = NullablePatch[int]{Set: ptr(4)}
	reg.Facts.TerminalSize = NullablePatch[TerminalSize]{Set: &size}
	got, _, err := s.RegisterRunner(ctx, reg)
	if err != nil {
		t.Fatal(err)
	}

	empty := ""
	working, unread, hasError := true, true, true
	clear := registration("facts", "shell", "/ignored", true, 10)
	clear.Facts = RunnerFacts{
		ConversationRef: &empty, WorkspaceRoot: &empty, Slug: &empty, ShellTitle: &empty, AdapterTitle: &empty, Subtitle: &empty,
		Working: &working, Unread: &unread, Error: &hasError,
		StartedAt: NullablePatch[UnixMillis]{Clear: true}, TerminalSize: NullablePatch[TerminalSize]{Clear: true},
	}
	got, result, err := s.RegisterRunner(ctx, clear)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConversationRef != "" || got.WorkspaceRoot != "" || got.Slug != "" || got.StartedAt != nil || got.ExitedAt != nil || got.ExitCode != nil || got.TerminalCols != nil || !got.Working || !got.Unread || !got.Error {
		t.Fatalf("tri-state merge=%#v", got)
	}
	if got.LastActivityAt == nil || *got.LastActivityAt != 10 || result.SessionVersion != 2 {
		t.Fatalf("activity/result=%#v %#v", got.LastActivityAt, result)
	}

	// Falling transitions do not bump activity; an older observation cannot
	// move it backwards. A same-generation final re-observation also does not
	// synthesize a lifecycle transition.
	f := false
	fall := registration("facts", "shell", "/ignored", true, 5)
	fall.Facts = RunnerFacts{Working: &f, Unread: &f, Error: &f}
	got, _, err = s.RegisterRunner(ctx, fall)
	if err != nil || got.LastActivityAt == nil || *got.LastActivityAt != 10 {
		t.Fatalf("fall=%#v err=%v", got, err)
	}
	death := registration("facts", "shell", "/ignored", false, 20)
	death.Facts = RunnerFacts{ExitedAt: NullablePatch[UnixMillis]{Set: ptr(UnixMillis(20))}}
	got, _, err = s.RegisterRunner(ctx, death)
	if err != nil || got.LastActivityAt == nil || *got.LastActivityAt != 10 {
		t.Fatalf("same-generation death=%#v err=%v", got, err)
	}
}

func TestRegisterRunnerGenerationProvenance(t *testing.T) {
	makeDead := func(t *testing.T) (*Store, Session) {
		t.Helper()
		ctx := context.Background()
		s := openKernelStore(t)
		registrationCatalog(t, s)
		live := registration("generation", "shell", "/one", true, 1)
		if _, _, err := s.RegisterRunner(ctx, live); err != nil {
			t.Fatal(err)
		}
		working := true
		active := registration("generation", "shell", "/ignored", true, 5)
		active.Facts = RunnerFacts{Working: &working}
		if _, _, err := s.RegisterRunner(ctx, active); err != nil {
			t.Fatal(err)
		}
		dead := registration("generation", "shell", "/ignored", false, 6)
		dead.Facts = RunnerFacts{
			Working:  ptr(false),
			ExitedAt: NullablePatch[UnixMillis]{Set: ptr(UnixMillis(6))},
			ExitCode: NullablePatch[int]{Set: ptr(6)},
		}
		got, _, err := s.RegisterRunner(ctx, dead)
		if err != nil {
			t.Fatal(err)
		}
		return s, got
	}

	t.Run("same-generation dead re-observation does not synthesize activity", func(t *testing.T) {
		s, before := makeDead(t)
		reg := registration("generation", "shell", "/ignored", false, 20)
		reg.Facts = RunnerFacts{
			ExitedAt: NullablePatch[UnixMillis]{Set: ptr(UnixMillis(6))},
			ExitCode: NullablePatch[int]{Set: ptr(6)},
		}
		got, result, err := s.RegisterRunner(context.Background(), reg)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastActivityAt == nil || *got.LastActivityAt != 5 || got.Version != before.Version || result.Changed {
			t.Fatalf("same generation=%#v result=%#v before=%#v", got, result, before)
		}
	})

	t.Run("new fast-dead generation replaces exit and bumps activity", func(t *testing.T) {
		s, before := makeDead(t)
		reg := registration("generation", "shell", "/ignored", false, 20)
		reg.NewGeneration = true
		reg.Facts = RunnerFacts{ExitedAt: NullablePatch[UnixMillis]{Set: ptr(UnixMillis(19))}}
		got, result, err := s.RegisterRunner(context.Background(), reg)
		if err != nil {
			t.Fatal(err)
		}
		if got.ExitedAt == nil || *got.ExitedAt != 19 || got.ExitCode != nil || got.LastActivityAt == nil || *got.LastActivityAt != 20 || got.Version != before.Version+1 || !result.SessionsDirty {
			t.Fatalf("new dead=%#v result=%#v", got, result)
		}
	})

	t.Run("new fast-dead generation without exit rejects atomically", func(t *testing.T) {
		s, before := makeDead(t)
		oldPlacement := *localPlacement(t, s, "generation")
		reg := registration("generation", "shell", "/two", false, 20)
		reg.NewGeneration = true
		_, _, err := s.RegisterRunner(context.Background(), reg)
		if !errors.Is(err, ErrGenerationExitRequired) {
			t.Fatalf("err=%v", err)
		}
		after, ok, getErr := s.Session(context.Background(), "generation")
		if getErr != nil || !ok || !reflect.DeepEqual(after, before) {
			t.Fatalf("after=%#v before=%#v ok=%v err=%v", after, before, ok, getErr)
		}
		if placement := localPlacement(t, s, "generation"); placement == nil || placement.project != oldPlacement.project || placement.pos != oldPlacement.pos {
			t.Fatalf("placement=%#v old=%#v", placement, oldPlacement)
		}
	})

	t.Run("new dead row without exit rejects", func(t *testing.T) {
		s := openKernelStore(t)
		reg := registration("fresh-dead", "shell", "/one", false, 20)
		if _, _, err := s.RegisterRunner(context.Background(), reg); !errors.Is(err, ErrGenerationExitRequired) {
			t.Fatalf("err=%v", err)
		}
		if _, ok, getErr := s.Session(context.Background(), "fresh-dead"); getErr != nil || ok {
			t.Fatalf("rejected new dead row committed: ok=%v err=%v", ok, getErr)
		}
	})

	t.Run("adapter mismatch wins over generation contract", func(t *testing.T) {
		s, before := makeDead(t)
		reg := registration("generation", "pi", "/ignored", false, 20)
		reg.NewGeneration = true
		_, result, err := s.RegisterRunner(context.Background(), reg)
		if !errors.Is(err, ErrAdapterMismatch) || result.SessionVersion != before.Version {
			t.Fatalf("err=%v result=%#v", err, result)
		}
	})

	t.Run("new generation resets generation-scoped facts", func(t *testing.T) {
		ctx := context.Background()
		s := openKernelStore(t)
		registrationCatalog(t, s)
		live := registration("reset", "shell", "/one", true, 1)
		live.Facts.Working = ptr(true)
		live.Facts.Error = ptr(true)
		live.Facts.Unread = ptr(true)
		live.Facts.StartedAt = NullablePatch[UnixMillis]{Set: ptr(UnixMillis(1))}
		if _, _, err := s.RegisterRunner(ctx, live); err != nil {
			t.Fatal(err)
		}
		// Prior generation dies mid-turn: working/started_at/error stay set.
		dead := registration("reset", "shell", "/ignored", false, 5)
		dead.Facts = RunnerFacts{ExitedAt: NullablePatch[UnixMillis]{Set: ptr(UnixMillis(5))}}
		if _, _, err := s.RegisterRunner(ctx, dead); err != nil {
			t.Fatal(err)
		}
		// Resume: the replacement generation observed none of those facts.
		resume := registration("reset", "shell", "/ignored", true, 9)
		resume.NewGeneration = true
		resume.Facts = RunnerFacts{}
		got, _, err := s.RegisterRunner(ctx, resume)
		if err != nil {
			t.Fatal(err)
		}
		if got.Working || got.Error || got.StartedAt != nil {
			t.Fatalf("generation-scoped facts leaked into replacement: %#v", got)
		}
		if !got.Unread {
			t.Fatalf("unread attention state must survive replacement: %#v", got)
		}
	})

	t.Run("new live generation clears old exit", func(t *testing.T) {
		s, before := makeDead(t)
		reg := registration("generation", "shell", "/ignored", true, 20)
		reg.NewGeneration = true
		got, result, err := s.RegisterRunner(context.Background(), reg)
		if err != nil {
			t.Fatal(err)
		}
		if got.ExitedAt != nil || got.ExitCode != nil || got.Version != before.Version+1 || !result.SessionsDirty {
			t.Fatalf("new live=%#v result=%#v", got, result)
		}
	})
}

func TestRegisterRunnerRematchesAndPreservesOrRemovesPlacement(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)
	for _, id := range []string{"a", "b"} {
		if _, _, err := s.RegisterRunner(ctx, registration(id, "shell", "/one", true, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if got := rootOrder(t, s, cat[0].ID); !reflect.DeepEqual(got, []string{"l:a", "l:b"}) {
		t.Fatalf("initial=%v", got)
	}

	// Same inputs preserve order exactly.
	_, r, err := s.RegisterRunner(ctx, registration("a", "shell", "/one", true, 2))
	if err != nil || r.Changed || !reflect.DeepEqual(rootOrder(t, s, cat[0].ID), []string{"l:a", "l:b"}) {
		t.Fatalf("preserve=%#v err=%v", r, err)
	}

	// Changed match inputs move atomically and append in the new project.
	_, r, err = s.RegisterRunner(ctx, registration("a", "shell", "/two/sub", true, 3))
	if err != nil || !r.WorldDirty || !r.SessionsDirty {
		t.Fatalf("move=%#v err=%v", r, err)
	}
	if got := rootOrder(t, s, cat[1].ID); !reflect.DeepEqual(got, []string{"l:a"}) {
		t.Fatalf("moved=%v", got)
	}

	// A changed input with no match removes the stale derived placement.
	_, r, err = s.RegisterRunner(ctx, registration("a", "shell", "/none", true, 4))
	if err != nil || !r.WorldDirty || localPlacement(t, s, "a") != nil {
		t.Fatalf("unmatch=%#v err=%v", r, err)
	}

	// An unplaced same-ID row is matched and appended on registration.
	_, r, err = s.RegisterRunner(ctx, registration("a", "shell", "/one", true, 5))
	if err != nil || !r.WorldDirty {
		t.Fatalf("replace=%#v err=%v", r, err)
	}
	if got := rootOrder(t, s, cat[0].ID); !reflect.DeepEqual(got, []string{"l:b", "l:a"}) {
		t.Fatalf("append=%v", got)
	}
}

func TestRegisterRunnerUnplacedRegistrationChangesPlacementNotRowVersion(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)
	got, _, err := s.RegisterRunner(ctx, registration("unplaced", "shell", "/one", true, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.database.ExecContext(ctx, `DELETE FROM project_placements WHERE local_session_id='unplaced'`); err != nil {
		t.Fatal(err)
	}

	reappear := registration("unplaced", "shell", "/ignored", true, 2)
	reappear.Facts = RunnerFacts{}
	got, result, err := s.RegisterRunner(ctx, reappear)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || result.SessionVersion != 1 || !result.Changed || !result.SessionsDirty || !result.WorldDirty {
		t.Fatalf("placement-only result=%#v session=%#v", result, got)
	}
	if p := localPlacement(t, s, "unplaced"); p == nil || p.project != int64(cat[0].ID) || p.pos != 0 {
		t.Fatalf("placement=%#v", p)
	}
}

func TestRegisterRunnerDismissedReappearanceAppendsAndKeepsPromotion(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)
	for _, id := range []string{"old", "kept"} {
		if _, _, err := s.RegisterRunner(ctx, registration(id, "shell", "/one", true, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetPromotion(ctx, "old", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.database.ExecContext(ctx, `UPDATE local_sessions SET dismissed_at_ms=9, row_version=row_version+1 WHERE id='old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.database.ExecContext(ctx, `DELETE FROM project_placements WHERE local_session_id='old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizePlacements(ctx, s.queries, nil); err != nil {
		t.Fatal(err)
	}

	got, r, err := s.RegisterRunner(ctx, registration("old", "shell", "/one", true, 10))
	if err != nil {
		t.Fatal(err)
	}
	if got.DismissedAt != nil || !got.PromotedToRoot || !r.SessionsDirty || !r.WorldDirty {
		t.Fatalf("reappear=%#v result=%#v", got, r)
	}
	if order := rootOrder(t, s, cat[0].ID); !reflect.DeepEqual(order, []string{"l:kept", "l:old"}) {
		t.Fatalf("order=%v", order)
	}
}

func TestRegisterRunnerChildBeforeParentRegroups(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)
	parent := SessionID("parent")
	child := registration("child", "shell", "/one", true, 1)
	child.LaunchParentID = &parent
	if _, _, err := s.RegisterRunner(ctx, child); err != nil {
		t.Fatal(err)
	}
	if got := rootOrder(t, s, cat[0].ID); !reflect.DeepEqual(got, []string{"l:child"}) {
		t.Fatalf("child root=%v", got)
	}
	if _, _, err := s.RegisterRunner(ctx, registration("parent", "shell", "/one", true, 2)); err != nil {
		t.Fatal(err)
	}
	if got := rootOrder(t, s, cat[0].ID); !reflect.DeepEqual(got, []string{"l:parent"}) {
		t.Fatalf("roots=%v", got)
	}
	if got := scopeOrder(t, s, cat[0].ID, "c:l:parent"); !reflect.DeepEqual(got, []string{"l:child"}) {
		t.Fatalf("children=%v", got)
	}
}

func TestRegisterRunnerMissingParentCycleRejectsExactly(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	b := SessionID("b")
	a := registration("a", "shell", "/one", true, 1)
	a.LaunchParentID = &b
	before, _, err := s.RegisterRunner(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	aID := SessionID("a")
	cycle := registration("b", "shell", "/one", true, 2)
	cycle.LaunchParentID = &aID
	if _, _, err = s.RegisterRunner(ctx, cycle); err == nil {
		t.Fatal("missing-parent cycle registration succeeded")
	}
	if _, ok, getErr := s.Session(ctx, "b"); getErr != nil || ok {
		t.Fatalf("cycle row ok=%v err=%v", ok, getErr)
	}
	after, ok, getErr := s.Session(ctx, "a")
	if getErr != nil || !ok || !reflect.DeepEqual(after, before) {
		t.Fatalf("existing child changed: after=%#v before=%#v", after, before)
	}
}

func TestRegisterRunnerDifferentProjectParentAndChildRemainRoots(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)
	parent := SessionID("parent")
	child := registration("child", "shell", "/two", true, 1)
	child.LaunchParentID = &parent
	if _, _, err := s.RegisterRunner(ctx, child); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegisterRunner(ctx, registration("parent", "shell", "/one", true, 2)); err != nil {
		t.Fatal(err)
	}
	if got := rootOrder(t, s, cat[0].ID); !reflect.DeepEqual(got, []string{"l:parent"}) {
		t.Fatalf("parent roots=%v", got)
	}
	if got := rootOrder(t, s, cat[1].ID); !reflect.DeepEqual(got, []string{"l:child"}) {
		t.Fatalf("child roots=%v", got)
	}
}

func TestRegisterRunnerMultipleChildrenBeforeParentKeepOrder(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)
	parent := SessionID("parent")
	for i, id := range []string{"first", "second", "third"} {
		child := registration(id, "shell", "/one", true, UnixMillis(i+1))
		child.LaunchParentID = &parent
		if _, _, err := s.RegisterRunner(ctx, child); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.RegisterRunner(ctx, registration("parent", "shell", "/one", true, 10)); err != nil {
		t.Fatal(err)
	}
	if got := scopeOrder(t, s, cat[0].ID, "c:l:parent"); !reflect.DeepEqual(got, []string{"l:first", "l:second", "l:third"}) {
		t.Fatalf("children=%v", got)
	}
}

func TestRegisterRunnerReferenceOnlyDoesNotMatch(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	if _, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{{Reference: &ProjectReference{PeerKey: "peer", Slug: "only"}}}, 1); err != nil {
		t.Fatal(err)
	}
	got, result, err := s.RegisterRunner(ctx, registration("reference", "shell", "/anything", true, 2))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || !result.SessionsDirty || result.WorldDirty || localPlacement(t, s, "reference") != nil {
		t.Fatalf("session=%#v result=%#v placement=%#v", got, result, localPlacement(t, s, "reference"))
	}
}

func TestRegisterRunnerRollbackExistingDismissedRematch(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	before, _, err := s.RegisterRunner(ctx, registration("existing", "shell", "/one", true, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.database.ExecContext(ctx, `UPDATE local_sessions SET dismissed_at_ms=9, row_version=row_version+1 WHERE id='existing'`); err != nil {
		t.Fatal(err)
	}
	before, _, err = s.Session(ctx, "existing")
	if err != nil {
		t.Fatal(err)
	}
	oldPlacement := *localPlacement(t, s, "existing")
	s.beforePlacementFinalize = func() error { return errors.New("injected existing") }
	reg := registration("existing", "shell", "/two", true, 10)
	if _, _, err = s.RegisterRunner(ctx, reg); err == nil {
		t.Fatal("fault succeeded")
	}
	after, ok, getErr := s.Session(ctx, "existing")
	if getErr != nil || !ok || !reflect.DeepEqual(after, before) {
		t.Fatalf("row rollback after=%#v before=%#v ok=%v err=%v", after, before, ok, getErr)
	}
	placement := localPlacement(t, s, "existing")
	if placement == nil || placement.project != oldPlacement.project || placement.scope != oldPlacement.scope || placement.pos != oldPlacement.pos {
		t.Fatalf("placement rollback=%#v old=%#v", placement, oldPlacement)
	}
}

func TestRegisterRunnerRollbackAtPlacementFault(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	s.beforePlacementFinalize = func() error { return errors.New("injected") }
	_, _, err := s.RegisterRunner(ctx, registration("rollback", "shell", "/one", true, 1))
	if err == nil {
		t.Fatal("fault succeeded")
	}
	if _, ok, getErr := s.Session(ctx, "rollback"); getErr != nil || ok {
		t.Fatalf("rolled-back row ok=%v err=%v", ok, getErr)
	}
	if p := localPlacement(t, s, "rollback"); p != nil {
		t.Fatalf("rolled-back placement=%#v", p)
	}
}
