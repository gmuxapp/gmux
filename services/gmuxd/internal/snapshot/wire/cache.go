package wire

import (
	"sync"

	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
)

// PeerSessionSource provides a point-in-time copy of every peer-session
// projection: network-peer rows (namespaced ID, Peer set, origin project
// stamps riding verbatim) and connected Local-peer rows (namespaced ID,
// Peer set; project stamps are overwritten here from the durable
// parent-owned placement join). Implementations must not perform I/O; the
// cache reads it once per applied batch. The same eventual-consistency
// contract as the composer's sources applies: every peer-session change
// must be followed by a MarkDirty so a skewed overlay is recomposed.
type PeerSessionSource interface {
	PeerSessions() []Session
}

// PeerSessionSourceFunc adapts a function to PeerSessionSource.
type PeerSessionSourceFunc func() []Session

func (f PeerSessionSourceFunc) PeerSessions() []Session { return f() }

// Frames is one conversion result: the wire events implied by an applied
// batch. Nil means "kind not (re)composed by this application".
type Frames struct {
	Sessions *SessionsPayload
	World    *WorldPayload
}

// Cache is the stateful conversion seam between the composer and its
// consumers (the SSE fan-out, GET /v1/sessions, GET /v1/projects). It
// retains the last payload of each kind so that:
//
//   - a sessions-only batch can still be flattened against the last known
//     Local-peer placements (FD-1 needs both kinds), and vice versa;
//   - a projects batch re-emits snapshot.sessions too: reorders and
//     placement changes arrive as world-dirty commits but change the
//     project_index stamps riding snapshot.sessions (production parity —
//     projects-update fired both coalescers);
//   - late subscribers get an immediate matched pair from Current()
//     (design §3.3 per-subscriber initial snapshot).
type Cache struct {
	conv  *Converter
	peers PeerSessionSource

	mu       sync.Mutex
	sessions *central.SessionsPayload
	projects *central.ProjectsPayload
}

// NewCache builds a Cache around a Converter and an optional peer-session
// overlay source.
func NewCache(conv *Converter, peers PeerSessionSource) *Cache {
	return &Cache{conv: conv, peers: peers}
}

// Apply folds one composer batch into the cache and returns the wire
// frames to broadcast. A batch carrying a projects payload recomposes the
// sessions frame as well (once a sessions payload exists) because
// placement stamps ride the session rows. Both frames are withheld until a
// sessions payload exists (review fable L-1): a projects-only FIRST batch
// must not emit a world whose owned projects appear session-less —
// unreachable post-bootstrap (phase 6.1 composes the matched pair first)
// but symmetric with Current()'s defensive posture.
func (c *Cache) Apply(b central.Batch) Frames {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b.Sessions != nil {
		c.sessions = b.Sessions
	}
	if b.Projects != nil {
		c.projects = b.Projects
	}
	if c.sessions == nil {
		return Frames{}
	}
	var peerRows []Session
	if c.peers != nil {
		peerRows = c.peers.PeerSessions()
	}
	var out Frames
	if b.Sessions != nil || b.Projects != nil {
		p := c.conv.Sessions(c.sessions, c.projects, peerRows)
		out.Sessions = &p
	}
	if b.Projects != nil && c.projects != nil {
		w := c.conv.World(c.sessions, c.projects, peerRows)
		out.World = &w
	}
	return out
}

// Current recomposes the cached state as a matched pair for one-shot reads
// and new-subscriber initial snapshots. Frames are nil for kinds never
// composed yet (pre-first-batch; post-bootstrap this cannot happen — the
// composer runs before listeners exist).
func (c *Cache) Current() Frames {
	c.mu.Lock()
	defer c.mu.Unlock()
	var peerRows []Session
	if c.peers != nil {
		peerRows = c.peers.PeerSessions()
	}
	var out Frames
	if c.sessions == nil {
		return out
	}
	p := c.conv.Sessions(c.sessions, c.projects, peerRows)
	out.Sessions = &p
	if c.projects != nil {
		w := c.conv.World(c.sessions, c.projects, peerRows)
		out.World = &w
	}
	return out
}
