package sessioncoord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

func TestProcessChildAutoReadSuccessfulFullExitPolicy(t *testing.T) {
	parent := centralstore.SessionID("parent01")
	other := centralstore.SessionID("parent02")
	child := centralstore.SessionID("child001")
	tests := []struct {
		name           string
		row            centralstore.Session
		liveParent     *centralstore.SessionID
		exitCode       int
		eventError     bool
		eventInterrupt bool
		want           bool
	}{
		{name: "live direct shell parent", row: centralstore.Session{ID: child, Adapter: "shell", ParentSessionID: &parent}, liveParent: &parent, want: true},
		{name: "idle parent still qualifies", row: centralstore.Session{ID: child, Adapter: "shell", ParentSessionID: &parent}, liveParent: &parent, want: true},
		{name: "current parent not launch provenance", row: centralstore.Session{ID: child, Adapter: "shell", ParentSessionID: &other, LaunchedFromSessionID: &parent}, liveParent: &parent},
		{name: "missing parent", row: centralstore.Session{ID: child, Adapter: "shell", ParentSessionID: &parent}},
		{name: "semantic child", row: centralstore.Session{ID: child, Adapter: "agent", ParentSessionID: &parent}, liveParent: &parent},
		{name: "status error", row: centralstore.Session{ID: child, Adapter: "shell", ParentSessionID: &parent, Error: true}, liveParent: &parent},
		{name: "interrupted", row: centralstore.Session{ID: child, Adapter: "shell", ParentSessionID: &parent, Interrupted: true}, liveParent: &parent},
		{name: "prior unread generation", row: centralstore.Session{ID: child, Adapter: "shell", ParentSessionID: &parent, Unread: true, UnreadToken: "older"}, liveParent: &parent},
		{name: "event error", row: centralstore.Session{ID: child, Adapter: "shell", ParentSessionID: &parent}, liveParent: &parent, eventError: true},
		{name: "event interrupted", row: centralstore.Session{ID: child, Adapter: "shell", ParentSessionID: &parent}, liveParent: &parent, eventInterrupt: true},
		{name: "nonzero exit", row: centralstore.Session{ID: child, Adapter: "shell", ParentSessionID: &parent}, liveParent: &parent, exitCode: 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.row.Version = 1
			d := newFakeDurable(1)
			d.session = func(id centralstore.SessionID) (centralstore.Session, bool, error) { return tc.row, id == child, nil }
			var got centralstore.RunnerObservation
			d.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
				got = obs
				return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 2}, nil
			}
			r := NewRegistry()
			r.install(registryEntry{Runtime: Runtime{SessionID: child, Generation: 1, RowVersion: 1}, dead: make(chan struct{})})
			if tc.liveParent != nil {
				r.install(registryEntry{Runtime: Runtime{SessionID: *tc.liveParent, Generation: 1, RowVersion: 1}, dead: make(chan struct{})})
			}
			c := New(r, nil, d, nil, nil, WithProcessChildAutoRead(func(s centralstore.Session) bool { return s.Adapter == "agent" }))
			unread, token, at, code, alive := true, "result-1", centralstore.UnixMillis(20), tc.exitCode, false
			facts := centralstore.RunnerFacts{
				Unread: &unread, UnreadToken: &token,
				ExitedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &at}, ExitCode: centralstore.NullablePatch[int]{Set: &code},
			}
			if tc.eventError {
				facts.Error = &tc.eventError
			}
			if tc.eventInterrupt {
				facts.Interrupted = &tc.eventInterrupt
			}
			c.apply(context.Background(), child, 1, RunnerEvent{ObservedAt: 20, Alive: &alive, Facts: facts})
			if got.SuppressUnread != tc.want {
				t.Fatalf("SuppressUnread=%v, want %v; observation=%+v", got.SuppressUnread, tc.want, got)
			}
		})
	}
}

func TestProcessChildAutoReadPolicyReadFaultStillCommitsAndRemoves(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "read error", err: errors.New("read fault")},
		{name: "missing row"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, child := centralstore.SessionID("parent01"), centralstore.SessionID("child001")
			d := newFakeDurable(1)
			d.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
				return centralstore.Session{}, false, tc.err
			}
			var applied centralstore.RunnerObservation
			d.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
				applied = obs
				return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 2}, nil
			}
			r := NewRegistry()
			r.install(registryEntry{Runtime: Runtime{SessionID: child, Generation: 1, RowVersion: 1}, dead: make(chan struct{})})
			r.install(registryEntry{Runtime: Runtime{SessionID: parent, Generation: 1, RowVersion: 1}, dead: make(chan struct{})})
			errs := &fakeErrorSink{}
			c := New(r, nil, d, nil, errs, WithProcessChildAutoRead(func(centralstore.Session) bool { return false }))
			c.apply(context.Background(), child, 1, successfulExitEvent())
			if applied.ID != child || applied.SuppressUnread {
				t.Fatalf("exit was dropped or suppressed after policy fault: %+v", applied)
			}
			if _, alive := r.current(child); alive {
				t.Fatal("dead child remained installed")
			}
			if tc.err != nil && errs.count() == 0 {
				t.Fatal("policy read fault was not reported")
			}
		})
	}
}

func successfulExitEvent() RunnerEvent {
	unread, token, at, code, alive := true, "result-1", centralstore.UnixMillis(20), 0, false
	return RunnerEvent{ObservedAt: 20, Alive: &alive, Facts: centralstore.RunnerFacts{
		Unread: &unread, UnreadToken: &token,
		ExitedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &at}, ExitCode: centralstore.NullablePatch[int]{Set: &code},
	}}
}

func TestSuppressionDecisionNeverTagsReplacementGeneration(t *testing.T) {
	parent, child := centralstore.SessionID("parent01"), centralstore.SessionID("child001")
	r := NewRegistry()
	r.install(registryEntry{Runtime: Runtime{SessionID: child, Generation: 1, RowVersion: 1}, dead: make(chan struct{})})
	r.install(registryEntry{Runtime: Runtime{SessionID: parent, Generation: 1, RowVersion: 1}, dead: make(chan struct{})})
	original := centralstore.Session{ID: child, Version: 1, Adapter: "shell", ParentSessionID: &parent}
	replacement := centralstore.Session{ID: child, Version: 3, Adapter: "shell", Unread: true, UnreadToken: "replacement-result"}
	replaced := false
	d := newFakeDurable(1)
	d.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		if replaced {
			return replacement, true, nil
		}
		return original, true, nil
	}
	d.applyResult = func(centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		replaced = true
		r.install(registryEntry{Runtime: Runtime{SessionID: child, Generation: 2, RowVersion: 3}, dead: make(chan struct{})})
		return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 2}, nil
	}
	c := New(r, nil, d, nil, nil, WithProcessChildAutoRead(func(centralstore.Session) bool { return false }))
	events, unsubscribe := c.SubscribeOutcomes()
	defer unsubscribe()
	c.apply(context.Background(), child, 1, successfulExitEvent())
	select {
	case outcome := <-events:
		if outcome.AttentionSuppressed || outcome.Generation != 2 || outcome.Session == nil || outcome.Session.UnreadToken != "replacement-result" {
			t.Fatalf("old decision tagged or obscured replacement outcome: %+v", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement invalidation outcome not published")
	}
}

func TestFastDeadRegistrationAutoReadPolicy(t *testing.T) {
	parent, child := centralstore.SessionID("parent01"), centralstore.SessionID("child001")
	for _, tc := range []struct {
		name        string
		adapter     string
		parentAlive bool
		errorFact   bool
		exitCode    int
		want        bool
	}{
		{name: "successful process with live parent", adapter: "shell", parentAlive: true, want: true},
		{name: "semantic agent", adapter: "agent", parentAlive: true},
		{name: "error fact", adapter: "shell", parentAlive: true, errorFact: true},
		{name: "nonzero exit", adapter: "shell", parentAlive: true, exitCode: 7},
		{name: "dead parent", adapter: "shell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unread, token, exited := true, "fast-result", centralstore.UnixMillis(20)
			active, interrupted := false, false
			reg := centralstore.RunnerRegistration{ID: child, Adapter: tc.adapter, Alive: false, CreatedAt: 1, ObservedAt: 20, ParentSessionID: &parent,
				Facts: centralstore.RunnerFacts{Active: &active, Error: &tc.errorFact, Interrupted: &interrupted, Unread: &unread, UnreadToken: &token,
					ExitedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &exited}, ExitCode: centralstore.NullablePatch[int]{Set: &tc.exitCode}}}
			client := newFakeClient(RunnerMeta{Registration: reg})
			dur := newFakeDurable(0)
			r := NewRegistry()
			if tc.parentAlive {
				r.install(registryEntry{Runtime: Runtime{SessionID: parent, Generation: 1, RowVersion: 1}, dead: make(chan struct{})})
			}
			coord := New(r, client, dur, nil, nil, WithProcessChildAutoRead(func(s centralstore.Session) bool { return s.Adapter == "agent" }))
			if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "fast"}); err != nil {
				t.Fatal(err)
			}
			if len(dur.registered) != 1 || dur.registered[0].SuppressUnread != tc.want {
				t.Fatalf("registration suppression=%v, want %v; reg=%+v", len(dur.registered) == 1 && dur.registered[0].SuppressUnread, tc.want, dur.registered)
			}
		})
	}
}

func TestFastDeadRegistrationUsesCurrentDurableParent(t *testing.T) {
	launchParent, currentParent, child := centralstore.SessionID("parent01"), centralstore.SessionID("parent02"), centralstore.SessionID("child001")
	c := New(NewRegistry(), nil, nil, nil, nil, WithProcessChildAutoRead(func(centralstore.Session) bool { return false }))
	c.registry.install(registryEntry{Runtime: Runtime{SessionID: launchParent, Generation: 1}, dead: make(chan struct{})})
	unread, token := true, "fresh"
	facts := centralstore.RunnerFacts{Unread: &unread, UnreadToken: &token}
	row := centralstore.Session{ID: child, Adapter: "shell", ParentSessionID: &currentParent, LaunchedFromSessionID: &launchParent}
	if c.supervisedSuccessfulExit(row, facts) {
		t.Fatal("launch parent liveness overrode dead current organizational parent")
	}
	c.registry.install(registryEntry{Runtime: Runtime{SessionID: currentParent, Generation: 1}, dead: make(chan struct{})})
	if !c.supervisedSuccessfulExit(row, facts) {
		t.Fatal("live current organizational parent did not qualify")
	}
	row.Unread, row.UnreadToken = true, "older"
	if c.supervisedSuccessfulExit(row, facts) {
		t.Fatal("fast-dead registration would clear a pre-existing unread result")
	}
}

func TestFastDeadRegistrationPolicyReadFailureFailsClosed(t *testing.T) {
	parent, child := centralstore.SessionID("parent01"), centralstore.SessionID("child001")
	unread, token, code, exited := true, "fast-result", 0, centralstore.UnixMillis(20)
	reg := centralstore.RunnerRegistration{ID: child, Adapter: "shell", Alive: false, CreatedAt: 1, ObservedAt: 20, ParentSessionID: &parent, Facts: centralstore.RunnerFacts{
		Unread: &unread, UnreadToken: &token, ExitCode: centralstore.NullablePatch[int]{Set: &code}, ExitedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &exited},
	}}
	client := newFakeClient(RunnerMeta{Registration: reg})
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{}, false, errors.New("read fault")
	}
	r := NewRegistry()
	r.install(registryEntry{Runtime: Runtime{SessionID: parent, Generation: 1}, dead: make(chan struct{})})
	errs := &fakeErrorSink{}
	coord := New(r, client, dur, nil, errs, WithProcessChildAutoRead(func(centralstore.Session) bool { return false }))
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "fast"}); err != nil {
		t.Fatal(err)
	}
	if len(dur.registered) != 1 || dur.registered[0].SuppressUnread {
		t.Fatalf("faulted policy did not fail closed: %+v", dur.registered)
	}
	if errs.count() == 0 {
		t.Fatal("policy read fault was not reported")
	}
}

func TestFastDeadRegistrationSuppressionRetryPreservesOlderOutcome(t *testing.T) {
	parent, child := centralstore.SessionID("parent01"), centralstore.SessionID("child001")
	unread, token, code, exited := true, "fresh-result", 0, centralstore.UnixMillis(20)
	reg := centralstore.RunnerRegistration{ID: child, Adapter: "shell", Alive: false, CreatedAt: 1, ObservedAt: 20, ParentSessionID: &parent, Facts: centralstore.RunnerFacts{
		Unread: &unread, UnreadToken: &token, ExitCode: centralstore.NullablePatch[int]{Set: &code}, ExitedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &exited},
	}}
	client := newFakeClient(RunnerMeta{Registration: reg})
	dur := newFakeDurable(0)
	dur.session = func(id centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: child, Version: 1, Adapter: "shell", ParentSessionID: &parent}, id == child, nil
	}
	calls := 0
	dur.registerResult = func(got centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		calls++
		if calls == 1 {
			if !got.SuppressUnread {
				t.Fatal("first registration did not attempt qualified suppression")
			}
			return centralstore.Session{}, centralstore.MutationResult{}, centralstore.ErrSuppressionWouldClearUnread
		}
		if got.SuppressUnread {
			t.Fatal("retry retained rejected suppression")
		}
		return centralstore.Session{ID: child, Version: 2, Adapter: "shell", ParentSessionID: &parent, Unread: true, UnreadToken: "older-result"},
			centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 2}, nil
	}
	r := NewRegistry()
	r.install(registryEntry{Runtime: Runtime{SessionID: parent, Generation: 1}, dead: make(chan struct{})})
	coord := New(r, client, dur, nil, nil, WithProcessChildAutoRead(func(centralstore.Session) bool { return false }))
	outcomes, unsubscribe := coord.SubscribeOutcomes()
	defer unsubscribe()
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "fast"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("registration calls=%d, want suppression plus unsuppressed retry", calls)
	}
	select {
	case outcome := <-outcomes:
		if outcome.AttentionSuppressed || outcome.Session == nil || !outcome.Session.Unread || outcome.Session.UnreadToken != "older-result" {
			t.Fatalf("retry outcome lost or tagged older result: %+v", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("registration retry outcome not published")
	}
}
