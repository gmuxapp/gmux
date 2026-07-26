package sessioncoord

import (
	"context"
	"errors"
	"sync"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// ErrConversationOwnedByLive marks a genuinely new fast-dead registration
// whose conversation is already covered by a live session of the same
// adapter. Production parity (store.resolveConversationTakeoverLocked's
// skip branch): the dead newcomer is a shadow of a conversation whose
// identity lives on in the live binder, so nothing durable is written and
// no registry change occurs. Same-ID re-registrations are exempt — an
// existing row's dead re-registration either already lost its row to the
// winner's eviction or genuinely owns it.
var ErrConversationOwnedByLive = errors.New("sessioncoord: conversation owned by a live session")

// ConversationInfo is what the owning adapter says about one opaque
// conversation ref: the conversation's own ID and the ancestor lineage it
// was resumed from (production adapter.ConversationDescriber).
type ConversationInfo struct {
	ID          string
	AncestorIDs []string
}

// ConversationResolver resolves opaque conversation refs to lineage. Adapter
// I/O: never called under the lifecycle mutex or inside a DB transaction. A
// nil resolver degrades takeover to ref equality, which is daemon-legal
// without adapter help (ADR 0022: refs are opaque but comparable within one
// adapter).
type ConversationResolver interface {
	DescribeConversation(ctx context.Context, adapter, ref string) (ConversationInfo, error)
}

// WithConversationTakeover enables conversation-lineage takeover on
// registration and reconciliation. resolver may be nil (ref-equality-only
// coverage). Production cutover configures this with the adapter registry's
// describers.
func WithConversationTakeover(resolver ConversationResolver) Option {
	return func(c *Coordinator) {
		c.takeover = true
		c.resolver = resolver
	}
}

// lineageEntry caches a successful describe for one (adapter, ref). Failures
// are deliberately not cached (production parity): caching a transient miss
// — e.g. a resumed transcript not yet on disk at first bind — would poison
// the ref and silently defeat takeover once the file lands. A successful
// describe is stable (ancestors never disappear).
type lineageEntry struct {
	id        string
	ancestors []string
}

// lineageCache is the coordinator's runtime-only (adapter, ref) → lineage
// map. It has its own lock because warming performs adapter I/O and must
// never hold the lifecycle mutex.
type lineageCache struct {
	mu      sync.Mutex
	entries map[string]lineageEntry
}

func lineageKey(adapter, ref string) string { return adapter + "\x00" + ref }

func (l *lineageCache) get(adapter, ref string) (lineageEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[lineageKey(adapter, ref)]
	return e, ok
}

// warm describes every not-yet-cached ref through the resolver. I/O; must be
// called without any coordinator lock. A nil resolver caches the empty
// lineage (nothing to learn later — ref equality still applies).
func (l *lineageCache) warm(ctx context.Context, resolver ConversationResolver, adapter string, refs []string) {
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if _, ok := l.get(adapter, ref); ok {
			continue
		}
		entry := lineageEntry{}
		if resolver != nil {
			info, err := resolver.DescribeConversation(ctx, adapter, ref)
			if err != nil {
				continue // not cached: retried on the next warm
			}
			entry = lineageEntry{id: info.ID, ancestors: info.AncestorIDs}
		}
		l.mu.Lock()
		if l.entries == nil {
			l.entries = make(map[string]lineageEntry)
		}
		l.entries[lineageKey(adapter, ref)] = entry
		l.mu.Unlock()
	}
}

// covers reports whether the owner's bound conversation is, or descends
// from, the other's: same opaque ref (legal without a describe), or the
// other's conversation ID appears in the owner's lineage (own ID +
// ancestors). Production store.coversConversation semantics.
func (l *lineageCache) covers(adapter, ownerRef, otherRef string) bool {
	if ownerRef == "" || otherRef == "" {
		return false
	}
	if ownerRef == otherRef {
		return true
	}
	other, ok := l.get(adapter, otherRef)
	if !ok || other.id == "" {
		return false
	}
	owner, ok := l.get(adapter, ownerRef)
	if !ok {
		return false
	}
	if owner.id == other.id {
		return true
	}
	for _, anc := range owner.ancestors {
		if anc == other.id {
			return true
		}
	}
	return false
}

// takeoverEvictions computes the loser set for a live registration: dead
// same-adapter rows (no installed or fenced generation) whose conversation
// the winner covers. Caller must hold the lifecycle mutex; sessions is the
// durable list read before the mutex — a registration that committed in
// between is missed here and converged by the next reconcile pass (the
// eviction is conditional at the observed version, so staleness is safe,
// never wrong).
func (c *Coordinator) takeoverEvictions(id centralstore.SessionID, adapter, ref string, sessions []centralstore.Session) []centralstore.TakeoverEviction {
	if ref == "" {
		return nil
	}
	var out []centralstore.TakeoverEviction
	for _, s := range sessions {
		if s.ID == id || s.Adapter != adapter || s.ConversationRef == "" {
			continue
		}
		if _, live := c.registry.current(s.ID); live {
			continue // live rows always coexist
		}
		if c.registry.fenced(s.ID) {
			continue // being replaced by a live registration right now
		}
		if !c.lineage.covers(adapter, ref, s.ConversationRef) {
			continue
		}
		out = append(out, centralstore.TakeoverEviction{ID: s.ID, Version: s.Version})
	}
	return out
}

// coveredByLive reports whether a live session of the same adapter covers
// ref (the dead-write-skip check). Caller must hold the lifecycle mutex.
func (c *Coordinator) coveredByLive(id centralstore.SessionID, adapter, ref string, sessions []centralstore.Session) bool {
	if ref == "" {
		return false
	}
	for _, s := range sessions {
		if s.ID == id || s.Adapter != adapter || s.ConversationRef == "" {
			continue
		}
		if _, live := c.registry.current(s.ID); !live {
			continue
		}
		if c.lineage.covers(adapter, s.ConversationRef, ref) {
			return true
		}
	}
	return false
}

// takeoverRefs collects every conversation ref of the given adapter from a
// durable session list, plus extra, deduplicated — the warm set for one
// registration or reconciliation pass.
func takeoverRefs(sessions []centralstore.Session, adapter string, extra ...string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(ref string) {
		if ref != "" && !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	for _, ref := range extra {
		add(ref)
	}
	for _, s := range sessions {
		if s.Adapter == adapter {
			add(s.ConversationRef)
		}
	}
	return out
}
