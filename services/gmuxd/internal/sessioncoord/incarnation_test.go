package sessioncoord

// incarnation_test.go — mutation-grade tests for socket identity in Runtime
// and for the identity-based orphan classification in ReapOrphans.
//
// The property under test is that a *pathname* never stands in for a
// *socket*. A pathname is reusable: a replacement runner binding the same
// pathname is a different socket and a different process, while the old
// generation may still be draining its own. Every consumer of the identity
// must also treat "unknown" as proving nothing, in whichever direction is
// conservative for that consumer (never suppress a probe, never kill).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/packages/socklease"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// listenSock binds a real Unix socket that never unlinks itself, so the test
// controls each rebind explicitly.
func listenSock(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	ul := ln.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	t.Cleanup(func() { _ = ul.Close() })
	return ul
}

func identOf(t *testing.T, path string) socklease.Ident {
	t.Helper()
	id, ok := socklease.StatSocket(path)
	if !ok {
		t.Fatalf("%s is not a socket", path)
	}
	return id
}

func metaFor(id centralstore.SessionID) RunnerMeta {
	return RunnerMeta{Registration: centralstore.RunnerRegistration{
		ID: id, Adapter: "shell", Alive: true, CreatedAt: 1, ObservedAt: 1,
	}, Incarnation: "incarnation-" + string(id)}
}

// rebind replaces the socket at path with one whose identity is provably
// different from prev.
//
// It does not unlink the old socket: a filesystem is free to hand the inode
// straight back, and the old identity's creation stamp comes from a coarse
// clock, so the "replacement" could be indistinguishable from the original --
// which would make this a test about nothing. Renaming the original aside keeps
// its inode linked, and therefore unusable, so the rebind necessarily lands on a
// different one. It fails rather than skips: an environment where a pathname
// cannot be given a new identity is one where the property under test cannot be
// observed at all.
func rebind(t *testing.T, old *net.UnixListener, path string, prev socklease.Ident) (*net.UnixListener, socklease.Ident) {
	t.Helper()
	_ = old.Close()
	for attempt := range 32 {
		aside := fmt.Sprintf("%s.parked-%d", path, attempt)
		if err := os.Rename(path, aside); err != nil {
			t.Fatalf("park %s aside: %v", path, err)
		}
		t.Cleanup(func() { _ = os.Remove(aside) })

		ln := listenSock(t, path)
		id := identOf(t, path)
		if !id.Same(prev) {
			return ln, id
		}
		// Same identity after all: leave this inode parked too, consuming what
		// the kernel just handed out, and try again.
		_ = ln.Close()
	}
	t.Fatalf("could not rebind %s to an identity distinct from %s", path, prev)
	return nil, socklease.Ident{}
}

// Mutation: drop the socket identity capture in Register.
func TestRegisterCarriesSocketIdentity(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "1zdou7bx.sock")
	listenSock(t, sockPath)
	want := identOf(t, sockPath)

	coord := New(nil, newFakeClient(metaFor("1zdou7bx")), newFakeDurable(0), &fakeDirtySink{}, &fakeErrorSink{})
	closeBarrier(t, coord)

	rt, err := coord.Register(context.Background(), RegisterRequest{Endpoint: sockPath})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !rt.Socket.Known() {
		t.Fatal("Runtime.Socket is unknown for a real socket endpoint")
	}
	if rt.Socket != want {
		t.Errorf("Runtime.Socket = %+v, want %+v", rt.Socket, want)
	}
}

// A synthetic (non-filesystem) endpoint has no identity, and must be reported
// as unknown rather than as some default that could accidentally compare equal
// to another unknown one.
func TestRegisterReportsUnknownIdentityForSyntheticEndpoint(t *testing.T) {
	coord := New(nil, newFakeClient(metaFor("1o949uu4")), newFakeDurable(0), &fakeDirtySink{}, &fakeErrorSink{})
	closeBarrier(t, coord)

	rt, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "synthetic-ep"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if rt.Socket.Known() {
		t.Fatalf("Runtime.Socket = %+v for a synthetic endpoint, want unknown", rt.Socket)
	}
}

// Mutation: capture the identity only once (before Subscribe, or only at
// install) instead of bracketing the runner I/O with settledIdent.
//
// If the pathname is rebound while a registration is in flight, the stream
// being installed and the inode the pathname names afterwards are not
// necessarily the same socket. Recording either one as fact would be a lie
// with teeth: Scan suppresses probes for the exact installed identity, so a
// wrong identity means a live replacement runner is never probed again.
func TestRegisterRefusesIdentityWhenPathnameMovesDuringRegistration(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "1gr9b1fc.sock")
	ln := listenSock(t, sockPath)
	before := identOf(t, sockPath)

	client := newFakeClient(metaFor("1gr9b1fc"))
	// Rebind the pathname in the middle of the registration's runner I/O.
	client.onMeta = func() { _, _ = rebind(t, ln, sockPath, before) }

	coord := New(nil, client, newFakeDurable(0), &fakeDirtySink{}, &fakeErrorSink{})
	closeBarrier(t, coord)

	rt, err := coord.Register(context.Background(), RegisterRequest{Endpoint: sockPath})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if rt.Socket.Known() {
		t.Fatalf("Runtime.Socket = %+v after the pathname was rebound mid-registration; "+
			"an unprovable identity must be reported as unknown", rt.Socket)
	}
}

// Mutation: revert ReapOrphans to comparing endpoint strings.
//
// The schedule: generation A is installed at pathname P and keeps draining its
// stream; P is rebound by a new process B that claims the same session id.
// B can never win registration (A is installed) and is invisible to a
// pathname-only comparison, so it would stay alive and unregistered forever.
func TestReapOrphansTerminatesSamePathnameDifferentSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "13kcxk20.sock")
	id := centralstore.SessionID("13kcxk20")
	ln := listenSock(t, sockPath)
	first := identOf(t, sockPath)

	control := &fakeControl{}
	coord := New(nil, newFakeClient(metaFor(id)), newFakeDurable(0), &fakeDirtySink{}, &fakeErrorSink{},
		WithRunnerControl(control))
	closeBarrier(t, coord)

	rt, err := coord.Register(context.Background(), RegisterRequest{Endpoint: sockPath})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if rt.Socket != first {
		t.Fatalf("installed identity %+v, want %+v", rt.Socket, first)
	}

	// B rebinds the pathname while A's generation stays installed.
	rebind(t, ln, sockPath, first)

	reaped, err := coord.ReapOrphans(context.Background(), []string{sockPath})
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != sockPath {
		t.Fatalf("reaped = %v, want [%s]; a rebound pathname is a different socket, "+
			"not the installed generation", reaped, sockPath)
	}
	if control.count() != 1 {
		t.Fatalf("Terminate called %d times, want 1", control.count())
	}
}

// The baseline the mutation above must not break: the installed generation's
// own endpoint is never a reap target.
func TestReapOrphansSkipsInstalledSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "1r6zxosb.sock")
	listenSock(t, sockPath)

	control := &fakeControl{}
	coord := New(nil, newFakeClient(metaFor("1r6zxosb")), newFakeDurable(0), &fakeDirtySink{}, &fakeErrorSink{},
		WithRunnerControl(control))
	closeBarrier(t, coord)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: sockPath}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reaped, err := coord.ReapOrphans(context.Background(), []string{sockPath})
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(reaped) != 0 || control.count() != 0 {
		t.Fatal("ReapOrphans terminated the installed generation")
	}
}

// Mutation: treat an unknown identity as "different" in ReapOrphans.
//
// This branch ends in killing a process, so an identity nobody could observe
// (a synthetic endpoint here; in production a pathname that moved under the
// probe) must never authorise the kill.
func TestReapOrphansSkipsUnknownIdentity(t *testing.T) {
	control := &fakeControl{}
	coord := New(nil, newFakeClient(metaFor("1flvcga6")), newFakeDurable(0), &fakeDirtySink{}, &fakeErrorSink{},
		WithRunnerControl(control))
	closeBarrier(t, coord)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "synthetic-ep"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reaped, err := coord.ReapOrphans(context.Background(), []string{"synthetic-ep"})
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(reaped) != 0 || control.count() != 0 {
		t.Fatal("ReapOrphans terminated a process whose socket identity was unknown")
	}
}

// Mutation: settle the probe identity before Meta only (drop the post-probe
// verification in ReapOrphans).
//
// If the pathname moves while the Meta probe is in flight, the answer came
// from one socket and the pathname now names another. Neither is provably an
// orphan, and this branch ends in killing a process.
func TestReapOrphansSkipsPathnameThatMovedUnderTheProbe(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "1iuknhfo.sock")
	id := centralstore.SessionID("1iuknhfo")
	ln := listenSock(t, sockPath)
	first := identOf(t, sockPath)

	control := &fakeControl{}
	client := newFakeClient(metaFor(id))
	coord := New(nil, client, newFakeDurable(0), &fakeDirtySink{}, &fakeErrorSink{},
		WithRunnerControl(control))
	closeBarrier(t, coord)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: sockPath}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// A different socket takes the pathname: this is now a reap candidate...
	ln2, second := rebind(t, ln, sockPath, first)
	// ...but it moves again while the reap probe is in flight.
	client.onMeta = func() {
		client.onMeta = nil
		rebind(t, ln2, sockPath, second)
	}

	reaped, err := coord.ReapOrphans(context.Background(), []string{sockPath})
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(reaped) != 0 || control.count() != 0 {
		t.Fatal("ReapOrphans killed a pathname whose identity changed under its own probe")
	}
}

// Mutation: drop the incarnation argument from the reap call in ReapOrphans
// (kill by pathname alone), or route the reap through Terminate.
//
// The schedule the reviews rejected:
//
//  1. Generation A is installed for session S at pathname P1.
//  2. Runner B answers at P2, claims S, and is classified as an orphan.
//  3. Before the kill lands, B exits and an unrelated runner C -- a different
//     session entirely -- binds P2.
//  4. The kill is addressed to P2, so it reaches C.
//
// Recoverability is not ownership: C is a live, healthy, unrelated runner and
// nothing about the classification was ever about it. The kill therefore
// carries the incarnation the classification was made about, and C's own
// /kill handler refuses it (pinned separately in ptyserver). Here we pin the
// half the coordinator owns: the expectation is sent, and it names B.
func TestReapOrphansNamesTheClassifiedRunnerInItsReap(t *testing.T) {
	installedID := centralstore.SessionID("1r6zxosb")
	orphanEP := "ep-orphan"

	client := &multiClient{metas: map[string]RunnerMeta{
		orphanEP: {
			Registration: centralstore.RunnerRegistration{ID: installedID, Adapter: "pi", Alive: true},
			Incarnation:  "incarnation-of-B",
		},
	}}
	control := &fakeControl{}
	coord := reapCoord(t, client, control)
	installLive(coord, installedID, "ep-installed")

	// C takes the pathname over the instant B has been classified, so by the
	// time the kill is sent the pathname belongs to an unrelated session.
	client.afterMeta = func(endpoint string) {
		client.mu.Lock()
		defer client.mu.Unlock()
		client.afterMeta = nil
		client.metas[endpoint] = RunnerMeta{
			Registration: centralstore.RunnerRegistration{ID: "1vqx6gk4", Adapter: "pi", Alive: true},
			Incarnation:  "incarnation-of-C",
		}
	}

	reaped, err := coord.ReapOrphans(context.Background(), []string{orphanEP})
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(reaped) != 1 {
		t.Fatalf("reaped = %v, want the classified orphan", reaped)
	}
	got := control.expectations()
	if len(got) != 1 {
		t.Fatalf("Terminate calls = %d, want 1", len(got))
	}
	if got[0] != "incarnation-of-B" {
		t.Fatalf("reap named %q, want the classified runner %q; an unnamed kill lands on whoever owns the pathname now",
			got[0], "incarnation-of-B")
	}
	// And it went through the conditional route, not the compatibility one: a
	// pre-protocol occupant obeys /kill regardless of any header.
	if routes := control.requestRoutes(); len(routes) != 1 || routes[0] != "reap" {
		t.Fatalf("control routes = %v, want [reap]", routes)
	}
}

// Mutation: treat ErrReapUnsupported as a failure (report it), or as a
// success (count it as reaped).
//
// An occupant that predates conditional reaping is left alone. That is the
// protocol working, not an incident: it must not be reported as an error, and
// it must not be counted as a reap.
func TestReapOrphansTreatsAnUnsupportedRouteAsADecline(t *testing.T) {
	installedID := centralstore.SessionID("1r6zxosb")
	client := &multiClient{metas: map[string]RunnerMeta{
		"ep-orphan": {
			Registration: centralstore.RunnerRegistration{ID: installedID, Adapter: "pi", Alive: true},
			Incarnation:  "incarnation-of-B",
		},
	}}
	control := &fakeControl{reapErr: fmt.Errorf("legacy occupant: %w", ErrReapUnsupported)}
	sink := &fakeErrorSink{}
	coord := New(nil, client, newFakeDurable(0), &fakeDirtySink{}, sink, WithRunnerControl(control))
	closeBarrier(t, coord)
	installLive(coord, installedID, "ep-installed")

	reaped, err := coord.ReapOrphans(context.Background(), []string{"ep-orphan"})
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("reaped = %v, want nothing: the occupant could not act on the request", reaped)
	}
	if sink.count() != 0 {
		t.Fatalf("a decline was reported as an error: %v", sink.count())
	}
}

// Mutation: reap candidates whose incarnation is unknown.
//
// A runner that predates the incarnation protocol cannot be named, so a kill
// against it cannot be bounded to the process the classification was about.
// Unknown means no kill.
func TestReapOrphansRefusesToKillAnUnidentifiableRunner(t *testing.T) {
	installedID := centralstore.SessionID("1r6zxosb")
	client := &multiClient{metas: map[string]RunnerMeta{
		// No Incarnation: a pre-protocol runner.
		"ep-legacy-orphan": {Registration: centralstore.RunnerRegistration{ID: installedID, Adapter: "pi", Alive: true}},
	}}
	control := &fakeControl{}
	coord := reapCoord(t, client, control)
	installLive(coord, installedID, "ep-installed")

	reaped, err := coord.ReapOrphans(context.Background(), []string{"ep-legacy-orphan"})
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(reaped) != 0 || control.count() != 0 {
		t.Fatalf("reaped an orphan the daemon cannot name: reaped=%v terminates=%d", reaped, control.count())
	}
}

// Mutation: drop the subscription/metadata incarnation comparison in Register.
//
// The ABA the reviews demonstrated: a bound AF_UNIX node can be hard-linked,
// so a pathname can name inode A, be replaced by B, and be restored to A --
// with every stat agreeing -- while Subscribe reached A and Meta reached B.
// Stat bracketing cannot see it; the runners' own identities can.
func TestRegisterRejectsAStreamAndMetadataFromDifferentRunners(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "1rthyoji.sock")
	listenSock(t, sockPath)

	client := newFakeClient(metaFor("1rthyoji"))
	// The stream came from one runner; the metadata answers as another. On
	// the filesystem this is the A -> B -> A restoration: same inode before
	// and after, two different processes in between.
	client.stream.incarnation = "incarnation-of-A"
	client.meta.Incarnation = "incarnation-of-B"

	coord := New(nil, client, newFakeDurable(0), &fakeDirtySink{}, &fakeErrorSink{})
	closeBarrier(t, coord)

	_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: sockPath})
	if !errors.Is(err, ErrRunnerIncarnationMismatch) {
		t.Fatalf("Register = %v, want ErrRunnerIncarnationMismatch", err)
	}
	// Nothing was installed: an unproven registration must not leave a trace.
	if snap := coord.registry.Snapshot(); len(snap) != 0 {
		t.Fatalf("registry = %+v, want empty", snap)
	}
}

// The hard-link ABA, executed on a real filesystem against the real Register.
//
// Without the incarnation comparison the stat bracket is satisfied -- the
// pathname names inode A before and after -- and the daemon would record a
// known socket identity for a generation whose stream and metadata came from
// different runners, then suppress every future probe of that pathname.
//
// Mutation: drop the incarnation comparison in Register.
func TestRegisterSurvivesHardlinkRestoredPathname(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "1krgq65n.sock")
	parked := filepath.Join(dir, "parked.sock")

	// A is bound at the endpoint, and hard-linked aside so its inode can come
	// back to the pathname later.
	lnA := listenSock(t, sockPath)
	identA := identOf(t, sockPath)
	if err := os.Link(sockPath, parked); err != nil {
		t.Skipf("this filesystem does not support hard-linking a bound socket: %v", err)
	}

	client := newFakeClient(metaFor("1krgq65n"))
	client.stream.incarnation = "incarnation-of-A"
	// Between Subscribe and Meta the pathname is B's, and B answers.
	client.onMeta = func() {
		client.onMeta = nil
		_ = lnA.Close()
		if err := os.Remove(sockPath); err != nil {
			t.Errorf("remove: %v", err)
		}
		lnB := listenSock(t, sockPath)
		client.meta.Incarnation = "incarnation-of-B"
		// ...and then B goes away and A's inode is restored to the pathname,
		// so the post-stat sees exactly what the pre-stat saw.
		_ = lnB.Close()
		if err := os.Remove(sockPath); err != nil {
			t.Errorf("remove B: %v", err)
		}
		if err := os.Link(parked, sockPath); err != nil {
			t.Errorf("restore A: %v", err)
		}
	}

	coord := New(nil, client, newFakeDurable(0), &fakeDirtySink{}, &fakeErrorSink{})
	closeBarrier(t, coord)

	_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: sockPath})

	// The ABA the schedule performs is at the level of the *numbers*: the
	// pathname names A's device and inode again.
	restored := identOf(t, sockPath)
	if restored.Dev != identA.Dev || restored.Ino != identA.Ino {
		t.Fatalf("the pathname was not restored to A's inode (%s -> %s); the schedule did not run",
			identA, restored)
	}
	// The full identity does not come back with it: link() touches the inode's
	// change time, so the stamp moved. That is a second, independent reason this
	// registration cannot claim an identity -- and it is worth noting that it is
	// a *filesystem* accident, not a guarantee: the guarantee is the runners'
	// own incarnations disagreeing, which is what this test asserts below and
	// which holds whatever the clock did.
	if restored.Same(identA) {
		t.Logf("this filesystem restored A's identity in full (%s); "+
			"only the incarnation comparison separates the two runners here", restored)
	}
	if !errors.Is(err, ErrRunnerIncarnationMismatch) {
		t.Fatalf("Register = %v, want ErrRunnerIncarnationMismatch: the stats agree, only the runners' own identities disagree", err)
	}
}

// A runner that predates the incarnation protocol registers normally, but the
// daemon refuses to claim it knows which socket that generation is subscribed
// to -- so it is probed on every sweep instead of being suppressed.
//
// Mutation: keep the stat-derived identity when the incarnation is unknown.
func TestRegisterWithoutIncarnationClaimsNoSocketIdentity(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "1u6d750s.sock")
	listenSock(t, sockPath)

	meta := metaFor("1u6d750s")
	meta.Incarnation = "" // pre-protocol runner
	client := newFakeClient(meta)

	coord := New(nil, client, newFakeDurable(0), &fakeDirtySink{}, &fakeErrorSink{})
	closeBarrier(t, coord)

	rt, err := coord.Register(context.Background(), RegisterRequest{Endpoint: sockPath})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if rt.Socket.Known() {
		t.Fatalf("Runtime.Socket = %+v for a runner that cannot be identified; "+
			"stat bracketing alone cannot rule out an ABA and must not license suppression", rt.Socket)
	}
	if rt.Incarnation != "" {
		t.Fatalf("Runtime.Incarnation = %q, want empty", rt.Incarnation)
	}
}

// Runtime identity is ephemeral, and the type system should be the thing that
// remembers that.
//
// An inode number, a creation timestamp and a process nonce describe this boot
// of this filesystem and this process. Persisted, they would be facts about a
// world that no longer exists -- and worse, a stored identity that happened to
// match a recycled inode would authorise exactly the destructive act this whole
// protocol exists to prevent. So neither the socket identity nor the incarnation
// may appear anywhere in the durable surface.
//
// Mutation: add Runtime.Socket or Runtime.Incarnation to
// centralstore.RunnerRegistration (or to RunnerFacts).
func TestEphemeralIdentityIsNeverPersisted(t *testing.T) {
	identType := reflect.TypeOf(socklease.Ident{})
	forbiddenNames := map[string]bool{"socket": true, "incarnation": true, "ident": true}

	var walk func(typ reflect.Type, path string, depth int)
	walk = func(typ reflect.Type, path string, depth int) {
		if depth > 4 || typ.Kind() != reflect.Struct {
			return
		}
		if typ == identType {
			t.Errorf("%s is a socklease.Ident: runtime identity must not reach durable state", path)
			return
		}
		for i := range typ.NumField() {
			f := typ.Field(i)
			name := path + "." + f.Name
			if forbiddenNames[strings.ToLower(f.Name)] {
				t.Errorf("%s names an ephemeral identity in a durable type", name)
			}
			ft := f.Type
			for ft.Kind() == reflect.Ptr || ft.Kind() == reflect.Slice {
				ft = ft.Elem()
			}
			walk(ft, name, depth+1)
		}
	}
	for _, durable := range []any{
		centralstore.RunnerRegistration{},
		centralstore.RunnerObservation{},
		centralstore.Session{},
	} {
		typ := reflect.TypeOf(durable)
		walk(typ, typ.Name(), 0)
	}

	// And the projection the snapshot composer sees, which is the only path out
	// of the registry, carries neither.
	rt := reflect.TypeOf(Runtime{})
	if _, found := rt.FieldByName("Socket"); !found {
		t.Fatal("Runtime.Socket is gone; this test is no longer guarding anything")
	}
	if _, found := rt.FieldByName("Incarnation"); !found {
		t.Fatal("Runtime.Incarnation is gone; this test is no longer guarding anything")
	}
}
