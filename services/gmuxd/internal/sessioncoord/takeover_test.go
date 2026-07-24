package sessioncoord

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// fakeResolver maps (adapter, ref) to lineage; unknown refs fail.
type fakeResolver struct {
	mu      sync.Mutex
	infos   map[string]ConversationInfo // lineageKey → info
	failAll bool
	calls   int
}

func (r *fakeResolver) DescribeConversation(_ context.Context, adapter, ref string) (ConversationInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.failAll {
		return ConversationInfo{}, errors.New("storage unreachable")
	}
	info, ok := r.infos[lineageKey(adapter, ref)]
	if !ok {
		return ConversationInfo{}, errors.New("unknown ref")
	}
	return info, nil
}

func deadSession(id centralstore.SessionID, adapter, ref string, version centralstore.RowVersion) centralstore.Session {
	exited := centralstore.UnixMillis(5)
	return centralstore.Session{ID: id, Version: version, Adapter: adapter, ConversationRef: ref, Command: []string{"sh"}, ExitedAt: &exited}
}

func liveMeta(id centralstore.SessionID, adapter, ref string) RunnerMeta {
	m := RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Adapter: adapter, Alive: true}}
	if ref != "" {
		r := ref
		m.Registration.Facts.ConversationRef = &r
	}
	return m
}

// installLive puts a live generation for id directly into the registry.
func installLive(c *Coordinator, id centralstore.SessionID, endpoint string) {
	c.registry.install(registryEntry{Runtime: Runtime{SessionID: id, Generation: 999, Endpoint: endpoint}, dead: make(chan struct{})})
}

func TestRegisterLiveEvictsCoveredDeadRows(t *testing.T) {
	winner := sid(1)
	resolver := &fakeResolver{infos: map[string]ConversationInfo{
		lineageKey("pi", "R"):  {ID: "cR", AncestorIDs: []string{"c2"}},
		lineageKey("pi", "R2"): {ID: "c2"},
		lineageKey("pi", "R3"): {ID: "c3"},
	}}
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{
			deadSession("equal-ref", "pi", "R", 3),  // covered by ref equality
			deadSession("ancestor", "pi", "R2", 4),  // covered by lineage
			deadSession("unrelated", "pi", "R3", 5), // not covered
			deadSession("other-adapter", "sh", "R", 6),
			deadSession("live-coexists", "pi", "R", 7),
			deadSession("no-ref", "pi", "", 8),
		}, nil
	}
	client := newFakeClient(liveMeta(winner, "pi", "R"))
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithConversationTakeover(resolver))
	installLive(coord, "live-coexists", "ep-live")

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(dur.registered) != 1 {
		t.Fatalf("registered=%d", len(dur.registered))
	}
	got := dur.registered[0].Evict
	want := map[centralstore.SessionID]centralstore.RowVersion{"equal-ref": 3, "ancestor": 4}
	if len(got) != len(want) {
		t.Fatalf("evictions=%#v", got)
	}
	for _, ev := range got {
		if v, ok := want[ev.ID]; !ok || v != ev.Version {
			t.Fatalf("unexpected eviction %#v", ev)
		}
	}
}

func TestRegisterDeadNewCoveredByLiveSkips(t *testing.T) {
	newcomer := sid(2)
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{
			{ID: "owner", Version: 1, Adapter: "pi", ConversationRef: "R", Command: []string{"pi"}},
		}, nil
	}
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{}, false, nil // genuinely new
	}
	meta := liveMeta(newcomer, "pi", "R")
	meta.Registration.Alive = false
	meta.Registration.Facts.ExitedAt = exitedAt(9)
	client := newFakeClient(meta)
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithConversationTakeover(nil))
	installLive(coord, "owner", "ep-owner")

	_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep2"})
	if !errors.Is(err, ErrConversationOwnedByLive) {
		t.Fatalf("err=%v", err)
	}
	if len(dur.registered) != 0 {
		t.Fatal("skip must not write durable state")
	}
	if _, live := coord.registry.current(newcomer); live {
		t.Fatal("skip must not install a registry entry")
	}
	if !client.stream.closed.Load() {
		t.Fatal("provisional stream must be closed")
	}
}

func TestRegisterDeadExistingRowIsNotSkipped(t *testing.T) {
	existing := sid(3)
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{
			{ID: "owner", Version: 1, Adapter: "pi", ConversationRef: "R", Command: []string{"pi"}},
			deadSession(existing, "pi", "R", 2),
		}, nil
	}
	dur.session = func(id centralstore.SessionID) (centralstore.Session, bool, error) {
		return deadSession(existing, "pi", "R", 2), true, nil
	}
	meta := liveMeta(existing, "pi", "R")
	meta.Registration.Alive = false
	meta.Registration.Facts.ExitedAt = exitedAt(9)
	client := newFakeClient(meta)
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithConversationTakeover(nil))
	installLive(coord, "owner", "ep-owner")

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep3"}); err != nil {
		t.Fatalf("existing row's dead re-registration must proceed: %v", err)
	}
	if len(dur.registered) != 1 {
		t.Fatal("expected the registration to commit")
	}
	if len(dur.registered[0].Evict) != 0 {
		t.Fatal("a dead registration must never carry evictions")
	}
}

func TestResolverFailureDegradesToRefEquality(t *testing.T) {
	winner := sid(4)
	resolver := &fakeResolver{failAll: true}
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{
			deadSession("equal-ref", "pi", "R", 3),
			deadSession("ancestor", "pi", "R2", 4), // would need lineage
		}, nil
	}
	client := newFakeClient(liveMeta(winner, "pi", "R"))
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithConversationTakeover(resolver))

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got := dur.registered[0].Evict
	if len(got) != 1 || got[0].ID != "equal-ref" {
		t.Fatalf("evictions=%#v", got)
	}
}

func TestTakeoverDisabledByDefault(t *testing.T) {
	winner := sid(5)
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		t.Fatal("takeover disabled: registration must not read the session list")
		return nil, nil
	}
	client := newFakeClient(liveMeta(winner, "pi", "R"))
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(dur.registered[0].Evict) != 0 {
		t.Fatal("no evictions without takeover")
	}
}

func TestTakeoverListFailureDegradesToNoEvictions(t *testing.T) {
	winner := sid(6)
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) { return nil, errors.New("db down") }
	client := newFakeClient(liveMeta(winner, "pi", "R"))
	errs := &fakeErrorSink{}
	coord := New(nil, client, dur, &fakeDirtySink{}, errs, WithConversationTakeover(nil))
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep1"}); err != nil {
		t.Fatalf("Register must still succeed: %v", err)
	}
	if len(dur.registered[0].Evict) != 0 {
		t.Fatal("no evictions when the list read failed")
	}
	if errs.count() != 1 {
		t.Fatalf("expected one reported error, got %d", errs.count())
	}
}

// TestLineageDescribeFailureNotCached pins the production rule: a failed
// describe is retried on the next warm; a success is cached and not
// re-described.
func TestLineageDescribeFailureNotCached(t *testing.T) {
	resolver := &fakeResolver{failAll: true}
	cache := &lineageCache{}
	cache.warm(context.Background(), resolver, "pi", []string{"R"})
	if _, ok := cache.get("pi", "R"); ok {
		t.Fatal("failure must not be cached")
	}
	resolver.mu.Lock()
	resolver.failAll = false
	resolver.infos = map[string]ConversationInfo{lineageKey("pi", "R"): {ID: "cR"}}
	resolver.mu.Unlock()
	cache.warm(context.Background(), resolver, "pi", []string{"R"})
	if e, ok := cache.get("pi", "R"); !ok || e.id != "cR" {
		t.Fatalf("entry=%#v ok=%v", e, ok)
	}
	before := resolver.calls
	cache.warm(context.Background(), resolver, "pi", []string{"R"})
	if resolver.calls != before {
		t.Fatal("cached ref must not be re-described")
	}
}
