package centralstore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
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

func TestConversationRebindClearsOutcomeMetadata(t *testing.T) {
	row := Session{ConversationRef: "A", Error: true, Interrupted: true, StatusReported: true}
	ref := "B"
	if err := mergeRunnerFacts(&row, RunnerFacts{ConversationRef: &ref}); err != nil {
		t.Fatal(err)
	}
	if row.ConversationRef != "B" || row.Active || row.Error || row.Interrupted || row.StatusReported {
		t.Fatalf("rebound row retained A outcome: %+v", row)
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
	live.Facts.Active = ptr(true)
	got, result, err := s.RegisterRunner(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.CreatedAt != 10 || got.ExitedAt != nil || !got.Active || !result.Changed || !result.SessionsDirty || !result.WorldDirty {
		t.Fatalf("live=%#v result=%#v", got, result)
	}
	if p := localPlacement(t, s, "live"); p == nil || p.project != int64(cat[0].ID) || p.pos != 0 {
		t.Fatalf("live placement=%#v", p)
	}

	dead := registration("dead", "shell", "/one", false, 20)
	dead.Facts.Active = ptr(false)
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

func TestRegisterRunnerRetainsConversationMetadataUntilAuthoritativeRebind(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	ref, title, subtitle, slug := "/conversations/a.jsonl", "Fix auth", "working", "fix-auth"
	first := registration("session", "pi", "/work", true, 1)
	first.Facts.ConversationRef = &ref
	first.Facts.AdapterTitle = &title
	first.Facts.Subtitle = &subtitle
	first.Facts.Slug = &slug
	if _, _, err := s.RegisterRunner(ctx, first); err != nil {
		t.Fatal(err)
	}

	// A replacement runner registers before its adapter hook binds. The wire
	// projection represents its empty /meta metadata as unobserved facts.
	unbound := registration("session", "pi", "/work", true, 2)
	unbound.NewGeneration = true
	got, _, err := s.RegisterRunner(ctx, unbound)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConversationRef != ref || got.AdapterTitle != title || got.Subtitle != subtitle || got.Slug != slug {
		t.Fatalf("unbound replacement lost conversation metadata: %#v", got)
	}

	// A transient same-ref parse represented as no positive display facts has
	// the same stale-while-revalidate semantics.
	same := registration("session", "pi", "/work", true, 3)
	same.Facts.ConversationRef = &ref
	got, _, err = s.RegisterRunner(ctx, same)
	if err != nil {
		t.Fatal(err)
	}
	if got.AdapterTitle != title || got.Subtitle != subtitle || got.Slug != slug {
		t.Fatalf("same-ref refresh lost conversation metadata: %#v", got)
	}

	// A positive same-ref refresh remains authoritative.
	renamed, renamedSlug := "Auth repaired", "auth-repaired"
	same.Facts.AdapterTitle = &renamed
	same.Facts.Slug = &renamedSlug
	got, _, err = s.RegisterRunner(ctx, same)
	if err != nil {
		t.Fatal(err)
	}
	if got.AdapterTitle != renamed || got.Slug != renamedSlug {
		t.Fatalf("positive refresh not applied: %#v", got)
	}

	// A different non-empty ref is an authoritative rebind. The production
	// pre-registration reducer drops A's metadata facts at this boundary, so
	// this ref-only update must clear A's cache by itself.
	refB := "/conversations/b.jsonl"
	rebound := registration("session", "pi", "/work", true, 4)
	rebound.Facts.ConversationRef = &refB
	got, _, err = s.RegisterRunner(ctx, rebound)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConversationRef != refB || got.AdapterTitle != "" || got.Subtitle != "" || got.Slug != "" || got.SlugBase != "" {
		t.Fatalf("authoritative rebind retained stale metadata: %#v", got)
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
	active, unread, hasError := true, true, true
	clear := registration("facts", "shell", "/ignored", true, 10)
	clear.Facts = RunnerFacts{
		ConversationRef: &empty, WorkspaceRoot: &empty, Slug: &empty, ShellTitle: &empty, AdapterTitle: &empty, Subtitle: &empty,
		Active: &active, Unread: &unread, Error: &hasError,
		StartedAt: NullablePatch[UnixMillis]{Clear: true}, TerminalSize: NullablePatch[TerminalSize]{Clear: true},
	}
	got, result, err := s.RegisterRunner(ctx, clear)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConversationRef != "" || got.WorkspaceRoot != "" || got.Slug != "" || got.StartedAt != nil || got.ExitedAt != nil || got.ExitCode != nil || got.TerminalCols != nil || !got.Active || !got.Unread || !got.Error {
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
	fall.Facts = RunnerFacts{Active: &f, Unread: &f, Error: &f}
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

func TestRegisterRunnerExitLifecycleValidatesMergedFinalState(t *testing.T) {
	exited := UnixMillis(2)
	code := 7
	tests := []struct {
		name                 string
		initialExited        *UnixMillis
		initialCode          *int
		facts                RunnerFacts
		valid                bool
		wantExited, wantCode bool
	}{
		{name: "set-code-without-timestamp", facts: RunnerFacts{ExitCode: NullablePatch[int]{Set: &code}}},
		{name: "clear-timestamp-retaining-code", initialExited: &exited, initialCode: &code, facts: RunnerFacts{ExitedAt: NullablePatch[UnixMillis]{Clear: true}}},
		{name: "set-both-atomically", facts: RunnerFacts{ExitedAt: NullablePatch[UnixMillis]{Set: &exited}, ExitCode: NullablePatch[int]{Set: &code}}, valid: true, wantExited: true, wantCode: true},
		{name: "clear-both-atomically", initialExited: &exited, initialCode: &code, facts: RunnerFacts{ExitedAt: NullablePatch[UnixMillis]{Clear: true}, ExitCode: NullablePatch[int]{Clear: true}}, valid: true},
		{name: "timestamp-only", facts: RunnerFacts{ExitedAt: NullablePatch[UnixMillis]{Set: &exited}}, valid: true, wantExited: true},
		{name: "add-code-to-timestamp", initialExited: &exited, facts: RunnerFacts{ExitCode: NullablePatch[int]{Set: &code}}, valid: true, wantExited: true, wantCode: true},
		{name: "clear-code-retain-timestamp", initialExited: &exited, initialCode: &code, facts: RunnerFacts{ExitCode: NullablePatch[int]{Clear: true}}, valid: true, wantExited: true},
		{name: "clear-timestamp-from-timestamp-only", initialExited: &exited, facts: RunnerFacts{ExitedAt: NullablePatch[UnixMillis]{Clear: true}}, valid: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openKernelStore(t)
			before, _, err := s.InsertSession(ctx, NewSession{ID: "merge", Adapter: "shell", Command: []string{}, CWD: "/", Remotes: map[string]string{}, CreatedAt: 1, ExitedAt: tc.initialExited, ExitCode: tc.initialCode})
			if err != nil {
				t.Fatal(err)
			}
			reg := registration("merge", "shell", "/", false, 3)
			reg.Facts = tc.facts
			got, _, err := s.RegisterRunner(ctx, reg)
			if !tc.valid {
				if err == nil || err.Error() != "centralstore: exit code requires exited timestamp" {
					t.Fatalf("merged validation error = %v", err)
				}
				after, ok, readErr := s.Session(ctx, "merge")
				if readErr != nil || !ok || !reflect.DeepEqual(after.ExitedAt, before.ExitedAt) || !reflect.DeepEqual(after.ExitCode, before.ExitCode) {
					t.Fatalf("invalid merge changed row: after=%#v before=%#v ok=%v err=%v", after, before, ok, readErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid merge rejected: %v", err)
			}
			if (got.ExitedAt != nil) != tc.wantExited || (got.ExitCode != nil) != tc.wantCode {
				t.Fatalf("merged state exited=%v code=%v", got.ExitedAt, got.ExitCode)
			}
		})
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
		unread := true
		active := registration("generation", "shell", "/ignored", true, 5)
		active.Facts = RunnerFacts{Unread: &unread}
		if _, _, err := s.RegisterRunner(ctx, active); err != nil {
			t.Fatal(err)
		}
		dead := registration("generation", "shell", "/ignored", false, 6)
		dead.Facts = RunnerFacts{
			Unread:   ptr(true),
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

	t.Run("new fast-dead generation replaces exit without bumping activity", func(t *testing.T) {
		s, before := makeDead(t)
		reg := registration("generation", "shell", "/ignored", false, 20)
		reg.NewGeneration = true
		reg.Facts = RunnerFacts{ExitedAt: NullablePatch[UnixMillis]{Set: ptr(UnixMillis(19))}}
		got, result, err := s.RegisterRunner(context.Background(), reg)
		if err != nil {
			t.Fatal(err)
		}
		// Output-only last_output_at: a resume seam / fast death is not
		// unseen output, so activity keeps the stamp from makeDead (5).
		if got.ExitedAt == nil || *got.ExitedAt != 19 || got.ExitCode != nil || got.LastActivityAt == nil || *got.LastActivityAt != 5 || got.Version != before.Version+1 || !result.SessionsDirty {
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
		live.Facts.Active = ptr(true)
		live.Facts.Error = ptr(true)
		live.Facts.Unread = ptr(true)
		live.Facts.StartedAt = NullablePatch[UnixMillis]{Set: ptr(UnixMillis(1))}
		if _, _, err := s.RegisterRunner(ctx, live); err != nil {
			t.Fatal(err)
		}
		// Prior generation dies mid-turn: active/started_at/error stay set.
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
		if got.Active || got.Error || got.StartedAt != nil {
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

func TestRegisterRunnerChildBeforeParentRegroups(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)
	parent := SessionID("parent")
	child := registration("child", "shell", "/one", true, 1)
	child.ParentSessionID = &parent
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
	a.ParentSessionID = &b
	before, _, err := s.RegisterRunner(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	aID := SessionID("a")
	cycle := registration("b", "shell", "/one", true, 2)
	cycle.ParentSessionID = &aID
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
	child.ParentSessionID = &parent
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
		child.ParentSessionID = &parent
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

func TestSessionSlugAllocationAndReplayIdempotence(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	base := "fix410-socket"

	first := registration("1zh4ov94", "pi", "/work", true, 10)
	first.Facts.Slug = &base
	got1, _, err := s.RegisterRunner(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	second := registration("1fg18fe5", "pi", "/work", false, 20)
	second.Facts.Slug = &base
	second.Facts.ExitedAt.Set = ptr(UnixMillis(21))
	got2, _, err := s.RegisterRunner(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if got1.Slug != base || got1.SlugBase != base || got2.Slug != base+"-2" || got2.SlugBase != base {
		t.Fatalf("allocations: first=%#v second=%#v", got1, got2)
	}

	// The runner continues to report its base proposal, not the allocated URL.
	out, err := s.ApplyRunnerObservation(ctx, RunnerObservation{ID: got2.ID, ObservedVersion: got2.Version, ObservedAt: 30, Facts: RunnerFacts{Slug: &base}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Changed || out.SessionVersion != got2.Version {
		t.Fatalf("same-base replay mutated row: %#v", out)
	}
	replayed, ok, err := s.Session(ctx, got2.ID)
	if err != nil || !ok || replayed.Slug != base+"-2" {
		t.Fatalf("replayed=%#v ok=%v err=%v", replayed, ok, err)
	}

	rename := "other"
	out, err = s.ApplyRunnerObservation(ctx, RunnerObservation{ID: got2.ID, ObservedVersion: replayed.Version, ObservedAt: 31, Facts: RunnerFacts{Slug: &rename}})
	if err != nil || !out.Changed {
		t.Fatalf("rename outcome=%#v err=%v", out, err)
	}
	renamed, _, _ := s.Session(ctx, got2.ID)
	if renamed.Slug != rename || renamed.SlugBase != rename {
		t.Fatalf("renamed=%#v", renamed)
	}
}

// Slug uniqueness is scoped per adapter (partial unique index on
// (adapter, slug)); the occupancy probe must not treat another adapter's
// slug as taken, and must still see every occupied slug on its own adapter.
func TestSessionSlugAllocationAdapterScoped(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	base := "same-name"

	a := registration("a", "pi", "/work", true, 10)
	a.Facts.Slug = &base
	gotA, _, err := s.RegisterRunner(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	b := registration("b", "claude", "/work", true, 20)
	b.Facts.Slug = &base
	gotB, _, err := s.RegisterRunner(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if gotA.Slug != base || gotB.Slug != base {
		t.Fatalf("cross-adapter slugs must not collide: a=%q b=%q", gotA.Slug, gotB.Slug)
	}
	c := registration("c", "pi", "/work", true, 30)
	c.Facts.Slug = &base
	gotC, _, err := s.RegisterRunner(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if gotC.Slug != base+"-2" {
		t.Fatalf("same-adapter collision must suffix: got %q", gotC.Slug)
	}
}

func TestConcurrentSessionSlugAllocation(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	base := "concurrent"
	const count = 12
	var wg sync.WaitGroup
	errCh := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reg := registration(fmt.Sprintf("%08d", i), "pi", "/work", true, UnixMillis(i+1))
			reg.Facts.Slug = &base
			_, _, err := s.RegisterRunner(ctx, reg)
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.Slug] {
			t.Fatalf("duplicate allocation %q", row.Slug)
		}
		seen[row.Slug] = true
	}
	if len(seen) != count || !seen[base] || !seen[base+"-2"] {
		t.Fatalf("allocations=%v", seen)
	}
}

func TestSessionSlugScopeIncludesAdapter(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	base := "same"
	for _, adapter := range []string{"pi", "shell"} {
		reg := registration(fmt.Sprintf("%07d1", len(adapter)), adapter, "/work", true, 1)
		reg.Facts.Slug = &base
		got, _, err := s.RegisterRunner(ctx, reg)
		if err != nil {
			t.Fatal(err)
		}
		if got.Slug != base {
			t.Fatalf("%s slug=%q", adapter, got.Slug)
		}
	}
}

func TestTakeoverSlugAllocationUsesPostEvictionNamespace(t *testing.T) {
	ctx := context.Background()
	t.Run("applied eviction releases base", func(t *testing.T) {
		s := openKernelStore(t)
		base := "foo"
		loserReg := registration("loser", "pi", "/work", false, 1)
		loserReg.Facts.Slug = &base
		loserReg.Facts.ExitedAt.Set = ptr(UnixMillis(2))
		loser, _, err := s.RegisterRunner(ctx, loserReg)
		if err != nil {
			t.Fatal(err)
		}
		winnerReg := registration("winner", "pi", "/work", true, 3)
		winnerReg.Facts.Slug = &base
		winnerReg.Evict = []TakeoverEviction{{ID: loser.ID, Version: loser.Version}}
		winner, _, err := s.RegisterRunner(ctx, winnerReg)
		if err != nil {
			t.Fatal(err)
		}
		if winner.Slug != base {
			t.Fatalf("winner slug=%q, want released base", winner.Slug)
		}
		if _, ok, err := s.Session(ctx, loser.ID); err != nil || ok {
			t.Fatalf("loser remains: ok=%v err=%v", ok, err)
		}
	})

	t.Run("stale eviction keeps base reserved", func(t *testing.T) {
		s := openKernelStore(t)
		base := "foo"
		loserReg := registration("loser", "pi", "/work", false, 1)
		loserReg.Facts.Slug = &base
		loserReg.Facts.ExitedAt.Set = ptr(UnixMillis(2))
		loser, _, err := s.RegisterRunner(ctx, loserReg)
		if err != nil {
			t.Fatal(err)
		}
		winnerReg := registration("winner", "pi", "/work", true, 3)
		winnerReg.Facts.Slug = &base
		winnerReg.Evict = []TakeoverEviction{{ID: loser.ID, Version: loser.Version + 1}}
		winner, _, err := s.RegisterRunner(ctx, winnerReg)
		if err != nil {
			t.Fatal(err)
		}
		if winner.Slug != base+"-2" {
			t.Fatalf("winner slug=%q, want suffixed slug", winner.Slug)
		}
		kept, ok, err := s.Session(ctx, loser.ID)
		if err != nil || !ok || kept.Slug != base {
			t.Fatalf("loser=%#v ok=%v err=%v", kept, ok, err)
		}
	})
}

func TestTakeoverSlugAllocationInteractingEvictionsNeverLeaveClearedSurvivor(t *testing.T) {
	for _, order := range []string{"parent-first", "child-first"} {
		t.Run(order, func(t *testing.T) {
			ctx := context.Background()
			s := openKernelStore(t)
			aBase, bBase := "parent-slug", "child-slug"
			aReg := registration("loser-a", "pi", "/work", false, 1)
			aReg.Facts.Slug = &aBase
			aReg.Facts.ExitedAt.Set = ptr(UnixMillis(2))
			a, _, err := s.RegisterRunner(ctx, aReg)
			if err != nil {
				t.Fatal(err)
			}

			bReg := registration("loser-b", "pi", "/work", false, 3)
			bReg.ParentSessionID = &a.ID
			bReg.Facts.Slug = &bBase
			bReg.Facts.ExitedAt.Set = ptr(UnixMillis(4))
			b, _, err := s.RegisterRunner(ctx, bReg)
			if err != nil {
				t.Fatal(err)
			}

			evA := TakeoverEviction{ID: a.ID, Version: a.Version}
			evB := TakeoverEviction{ID: b.ID, Version: b.Version}
			evictions := []TakeoverEviction{evA, evB}
			if order == "child-first" {
				evictions = []TakeoverEviction{evB, evA}
			}
			winnerReg := registration("winner", "pi", "/work", true, 5)
			winnerReg.Facts.Slug = &bBase
			winnerReg.Evict = evictions
			winner, _, err := s.RegisterRunner(ctx, winnerReg)
			if err != nil {
				t.Fatal(err)
			}
			if winner.Slug != bBase {
				t.Fatalf("winner slug=%q, want %q", winner.Slug, bBase)
			}
			for _, loser := range []Session{a, b} {
				if got, ok, err := s.Session(ctx, loser.ID); err != nil || ok {
					t.Fatalf("loser %s survived: %#v ok=%v err=%v", loser.ID, got, ok, err)
				}
			}
		})
	}
}

func TestTakeoverSlugAllocationTopologicalEvictions(t *testing.T) {
	tests := []struct {
		name  string
		order []string
	}{
		{name: "interleaved-unrelated", order: []string{"a", "x", "b", "y", "c"}},
		{name: "deep-reversed-with-barriers", order: []string{"a", "y", "b", "x", "c"}},
		{name: "deep-scrambled", order: []string{"x", "c", "a", "y", "b"}},
		{name: "duplicates-and-missing", order: []string{"a", "missing", "x", "b", "b", "c", "y"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openKernelStore(t)
			registered := map[string]Session{}
			registerLoser := func(id, slug string, parent *SessionID, at UnixMillis) {
				t.Helper()
				reg := registration(id, "pi", "/work", false, at)
				reg.ParentSessionID = parent
				reg.Facts.Slug = &slug
				reg.Facts.ExitedAt.Set = ptr(at + 1)
				row, _, err := s.RegisterRunner(ctx, reg)
				if err != nil {
					t.Fatal(err)
				}
				registered[id] = row
			}
			registerLoser("a", "slug-a", nil, 1)
			aID := registered["a"].ID
			registerLoser("b", "slug-b", &aID, 3)
			bID := registered["b"].ID
			registerLoser("c", "slug-c", &bID, 5)
			registerLoser("x", "slug-x", nil, 7)
			registerLoser("y", "slug-y", nil, 9)

			evictions := make([]TakeoverEviction, 0, len(tc.order))
			for _, id := range tc.order {
				if id == "missing" {
					evictions = append(evictions, TakeoverEviction{ID: "missing", Version: 1})
					continue
				}
				row := registered[id]
				evictions = append(evictions, TakeoverEviction{ID: row.ID, Version: row.Version})
			}
			base := "slug-c"
			winnerReg := registration("winner", "pi", "/work", true, 20)
			winnerReg.Facts.Slug = &base
			winnerReg.Evict = evictions
			winner, _, err := s.RegisterRunner(ctx, winnerReg)
			if err != nil {
				t.Fatal(err)
			}
			if winner.Slug != base {
				t.Fatalf("winner slug=%q, want released %q", winner.Slug, base)
			}
			for id, loser := range registered {
				if got, ok, err := s.Session(ctx, loser.ID); err != nil || ok {
					t.Fatalf("loser %s survived: %#v ok=%v err=%v", id, got, ok, err)
				}
			}
		})
	}
}

func TestOrderTakeoverEvictionsRejectsCorruptParentCycle(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	for i, id := range []string{"cycle-a", "cycle-b"} {
		reg := registration(id, "pi", "/work", false, UnixMillis(i+1))
		reg.Facts.ExitedAt.Set = ptr(UnixMillis(i + 2))
		if _, _, err := s.RegisterRunner(ctx, reg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.database.ExecContext(ctx, `DROP TRIGGER local_sessions_parent_no_cycle_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.database.ExecContext(ctx, `UPDATE local_sessions SET parent_session_id = CASE id WHEN 'cycle-a' THEN 'cycle-b' ELSE 'cycle-a' END`); err != nil {
		t.Fatal(err)
	}
	_, err := orderTakeoverEvictions(ctx, s.queries, []TakeoverEviction{{ID: "cycle-a", Version: 1}, {ID: "cycle-b", Version: 1}})
	if err == nil {
		t.Fatal("corrupt launch-parent cycle was accepted")
	}
}

func TestRegisterRunnerSameIDPreservesHistoryAndNoop(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)
	parent := SessionID("original-parent")
	first := registration("same", "shell", "/one", false, 10)
	first.ParentSessionID = &parent
	first.Facts.ExitedAt = NullablePatch[UnixMillis]{Set: ptr(UnixMillis(12))}
	first.Facts.ExitCode = NullablePatch[int]{Set: ptr(3)}
	got, _, err := s.RegisterRunner(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetSessionParent(ctx, "same", nil); err != nil {
		t.Fatal(err)
	}
	beforePlacement := localPlacement(t, s, "same")
	beforePos, beforeProject := beforePlacement.pos, beforePlacement.project

	otherParent := SessionID("replacement-parent")
	resume := registration("same", "shell", "/one", true, 99)
	resume.ParentSessionID = &otherParent
	got, result, err := s.RegisterRunner(ctx, resume)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt != 10 || got.ParentSessionID != nil || got.ExitedAt != nil || got.ExitCode != nil {
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
