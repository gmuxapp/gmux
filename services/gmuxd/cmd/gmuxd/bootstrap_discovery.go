package main

// Discovery classification: what a periodic scan does with each endpoint, and
// what it is allowed to say about it.
//
// Two problems shaped this file. The first is work: a healthy, unchanged
// fleet was re-subscribing and re-probing every runner every 30 seconds, only
// for the coordinator to reject each registration because that generation was
// already installed. The second is noise: every one of those rejections, plus
// every refused stale socket, was logged on every tick -- 96 endpoints
// produced ~56 MiB of daemon log per day, which is also how a full disk turns
// into a corrupted SQLite database.
//
// The fix for both is to compare socket identities rather than pathnames. An
// endpoint whose socket is *exactly* the one an installed generation is
// subscribed to is skipped: no Subscribe, no Meta, no log line. Anything else
// -- an unknown identity, a rebound pathname, a second endpoint claiming an
// installed session id -- is still probed and still visible, because those are
// the cases that indicate something is actually wrong.
//
// Diagnostics are reported on transition. An endpoint that fails the same way
// on every tick is reported once; when it changes, or recovers, or is reaped,
// that is reported too. Diagnostic state is keyed by socket identity, so a
// rebound pathname starts with a clean slate and its first failure is loud.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"syscall"

	"github.com/gmuxapp/gmux/packages/socklease"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

// diagKey identifies one diagnosable thing: a pathname *and* the socket it
// named. Keying on the pathname alone would let a stale "unreachable" verdict
// silence the first failure of the replacement runner that rebound it.
type diagKey struct {
	endpoint string
	socket   socklease.Ident
}

// endpointDiag remembers the last reported message per endpoint incarnation so
// discovery can report transitions instead of steady states.
type endpointDiag struct {
	mu   sync.Mutex
	last map[diagKey]string
}

func newEndpointDiag() *endpointDiag { return &endpointDiag{last: make(map[diagKey]string)} }

// note records msg for key and reports whether it is new information.
func (d *endpointDiag) note(key diagKey, msg string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if prev, ok := d.last[key]; ok && prev == msg {
		return false
	}
	d.last[key] = msg
	return true
}

// clearEndpoint forgets everything being reported about a pathname and
// reports whether there was anything, i.e. whether this is a recovery worth a
// line.
//
// It clears by pathname rather than by incarnation on purpose. A pathname that
// registers successfully is healthy whichever socket now answers there, and
// the entry for the incarnation that used to fail would otherwise sit in the
// map until the pathname vanished entirely. Failures are still *recorded* per
// incarnation, so a new socket's first failure is never mistaken for its
// predecessor's steady state.
func (d *endpointDiag) clearEndpoint(ep string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	found := false
	for key := range d.last {
		if key.endpoint == ep {
			delete(d.last, key)
			found = true
		}
	}
	return found
}

// retain drops every entry whose pathname is no longer being enumerated. Reaped
// and vanished endpoints never come back to clear themselves, so without this
// the map grows for the life of the daemon.
func (d *endpointDiag) retain(endpoints []string) {
	live := make(map[string]struct{}, len(endpoints))
	for _, ep := range endpoints {
		live[ep] = struct{}{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for key := range d.last {
		if _, ok := live[key.endpoint]; !ok {
			delete(d.last, key)
		}
	}
}

func (d *endpointDiag) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.last)
}

// notice reports something that is not an error but should be visible exactly
// once: a stale socket being reaped, or an endpoint recovering.
func (b *Bootstrap) notice(ctx context.Context, format string, args ...any) {
	if b.cfg.Notices != nil {
		b.cfg.Notices(ctx, fmt.Sprintf(format, args...))
		return
	}
	log.Printf("gmuxd: "+format, args...)
}

func (b *Bootstrap) report(ctx context.Context, err error) {
	if b.cfg.Errors != nil {
		b.cfg.Errors.Error(ctx, err)
	}
}

// installedSockets is the set of socket identities the registry currently has
// installed and subscribed. The registry is the authority: nothing caches this
// alongside it, so an entry that dies stops suppressing work immediately.
func (b *Bootstrap) installedSockets() map[socklease.Ident]struct{} {
	return b.Registry.InstalledSockets()
}

// endpointIdent is the current physical identity of an endpoint pathname, or
// the unknown identity for anything that is not a filesystem socket.
func endpointIdent(ep string) socklease.Ident {
	id, ok := socklease.StatSocket(ep)
	if !ok {
		return socklease.Ident{}
	}
	return id
}

// classifyRegister turns one Register outcome into at most one log line.
//
// phase distinguishes the startup convergence sweep from the periodic scan, so
// the two call sites remain separately identifiable in the log (and separately
// pinnable in tests).
func (b *Bootstrap) classifyRegister(ctx context.Context, phase, ep string, ident socklease.Ident, rt sessioncoord.Runtime, err error) {
	key := diagKey{endpoint: ep, socket: ident}

	if err == nil {
		if b.diag.clearEndpoint(ep) {
			b.notice(ctx, "%s: %s recovered", phase, ep)
		}
		return
	}

	if errors.Is(err, sessioncoord.ErrGenerationActive) {
		// The coordinator returns the installed generation's runtime with this
		// error. If that generation is subscribed to *this exact socket*, we
		// simply lost a harmless race with the runner-initiated registration
		// of the same runner: expected, and not worth a line.
		//
		// Anything else is a genuine collision -- a different socket claiming
		// a session id that is already installed -- and must stay visible.
		// Note the deliberate asymmetry: an unknown identity is not treated as
		// a match, so an unidentifiable collision is reported rather than
		// silently swallowed.
		if rt.Socket.Known() && rt.Socket.Same(ident) {
			if b.diag.clearEndpoint(ep) {
				b.notice(ctx, "%s: %s recovered", phase, ep)
			}
			return
		}
		if b.diag.note(key, err.Error()) {
			b.report(ctx, fmt.Errorf("%s: %s claims session %s, which is already installed at %s: %w",
				phase, ep, rt.SessionID, rt.Endpoint, err))
		}
		return
	}

	if errors.Is(err, syscall.ECONNREFUSED) {
		// Nothing is listening. This is the population that accumulated: a
		// runner died without unlinking its pathname. Try to reap it under the
		// ownership lease; if the reaper declines, say why, once.
		outcome := reapStaleSocket(ep)
		if outcome.Reaped {
			b.diag.clearEndpoint(ep)
			b.notice(ctx, "%s: reaped stale socket %s", phase, ep)
			return
		}
		if b.diag.note(key, "refused: "+outcome.Reason) {
			b.report(ctx, fmt.Errorf("%s: %s refuses connections but was not reaped (%s)", phase, ep, outcome.Reason))
		}
		return
	}

	if b.diag.note(key, err.Error()) {
		b.report(ctx, fmt.Errorf("%s register %s: %w", phase, ep, err))
	}
}
