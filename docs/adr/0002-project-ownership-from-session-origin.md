# ADR 0002: Project ownership from session origin

**Status:** Proposed
**Date:** 2026-04-25
**Related:** ADR 0001 (Snapshot push protocol)

## Context

A gmux project is a user-curated grouping of sessions identified by
slug, with match rules (paths, git remotes) and an ordered
`Sessions[]` array that controls sidebar membership and order.
Project state is per-host, persisted in
`<state-dir>/projects.json`.

Today, every gmuxd that sees a session decides independently which
of its own projects (if any) the session belongs to. The local
auto-assigner runs against every session in the local store,
including sessions owned by network peers. Symptoms:

1. **The "+" button silently launches on the wrong host.**
   `LaunchButton` on a folder reads `folder.launchCwd` and POSTs
   `/v1/launch` without a `peer` field. A folder visually showing
   five peer-owned sessions still launches a sixth on the local
   host. Users have no consistent mental model for where a launch
   lands.
2. **Mutations don't propagate.** Reordering or dismissing a
   session from a project is purely local: each host's
   `projects.json` is independent, and the SSE protocol carries no
   cross-host project signal. Users editing the sidebar on host A
   see no effect on host B's view.
3. **Match rules carry cross-host conceits.** `MatchRule.Hosts`,
   `NormalizeRemote`, and `~`-path expansion all exist in part to
   make a single project rule "work" on multiple hosts. The
   conceit is that `~/dev/foo` on every host is the same project,
   when it is in fact one filesystem checkout per host.
4. **Stale peer keys accumulate.** Each local `projects.json`
   accumulates keys for peer-owned sessions via auto-assignment.
   These keys stay even after the peer goes away; over time the
   file becomes cluttered with phantom entries.

### The mental model we're after

A project has **state**: a slug, match rules, an ordered list of
session keys. State lives somewhere. The cleanest, most honest
placement is: state lives on the host that curates it, full stop.
That host is also the one with a working copy of the source, the
one whose runners produce sessions for the project, and the one
whose user actually edits `projects.json`.

From that, two consequences fall out:

- **Two hosts that happen to use the slug `gmux` for their
  respective projects are not the "same project".** They have
  independent rules, independent membership arrays, and (usually)
  independent working copies pointing at independent remotes.
  Merging them in any viewer's sidebar is a per-viewer fiction
  with no authority for ordering or membership.
- **A viewer that wants to manipulate a remote project is
  controlling that project's owner**, not maintaining a local
  shadow. The viewer's `projects.json` should hold no state about
  projects it does not own. Mutations cross the wire to the owner;
  the owner's snapshot reflects the result back to all viewers.

This ADR commits to that model: **a project lives on exactly one
host; viewers render and steer; they do not co-author.**

### The case for a free-game disclaim

Not every gmuxd is a curator. Devcontainers, CI runners, fresh
hosts, and short-lived utility nodes produce sessions but never
have a human curating their `projects.json`. Their natural state
is empty.

We could introduce an explicit "headless" flag (config or CLI) to
classify such hosts and gate behaviour. We deliberately don't
(see Alternative E). Instead, the wire bit `project_slug == ""`
already encodes the only thing a viewer needs to know: "the
origin is not claiming this session." An origin with an empty
`projects.json` disclaims every session by default, which makes
it emergently "headless" without any new config. A curator host
that happens to have a one-off shell in `/tmp` produces the same
disclaim signal for that one session, and that's correct: the
user told it nothing about that session, so anyone with a
matching rule is welcome to file it.

Disclaimed sessions are *free game*: viewers run their own match
rules against them. If a viewer's rules adopt one, the viewer
adds the key to its own `projects.json` (its intent, its file).
If nothing adopts, the session falls to the discovered /
unclaimed UI, exactly like a local orphan. This preserves the
current zero-config experience for devcontainers and fresh hosts
while still letting curator hosts keep their own house in order.

## Decision

Project membership for a session is owned by the **origin host**
(the gmuxd whose runner is executing the session). Each session
carries its origin's project assignment as part of its wire data.
Receivers render based on this assignment without re-running their
own match rules against peer-owned sessions.

When the origin disclaims the session (no matching project, or no
projects configured at all), receivers fall back to their own match
rules and treat the session as local-adoptable. This preserves the
auto-discovery behaviour for the common no-projects-on-peer case
(devcontainers, fresh hosts) without introducing a separate
"headless host" classification: an origin with no projects
disclaims by definition, and the disclaim alone is enough.

### Wire shape

Two new fields on `store.Session`, populated only by the origin and
only as a pair:

- `project_slug: string` — empty means "origin disclaims this
  session"; non-empty is the slug of the origin's project that
  claims it.
- `project_index: int` — 0-based position in that project's
  `Sessions[]` array on the origin. Only meaningful when
  `project_slug != ""`.

These travel with the session in `snapshot.sessions` (per ADR 0001).

### Origin-side stamping (`Reconcile`)

A single `Reconcile()` function on the origin walks
`projects.json.Items[]` in order; for each `key` in `Sessions[]`
it stamps the matching `store.Session` with `(slug, index)`. Sessions
not in any array are stamped `("", 0)`.

`Reconcile()` is the only writer of these two fields on the origin.
Triggers:

- After every `projectMgr.Update(...)` (any mutation to
  `projects.json`).
- After every `sessions.Upsert` / `sessions.Update` for
  origin-owned sessions (i.e., `Peer == ""`).

For peer-owned sessions, the origin's stamps are received over the
wire and stored as-is. The local `Reconcile` does not touch them.

### Sidebar rendering rule

```
for each session s in store:
  if s.project_slug != "":
    folder = (s.peer, s.project_slug)         // origin-claimed
    sort key = (s.peer, s.project_index)
  else:
    folder = matchAgainstLocalProjects(s)     // disclaimed: viewer-owned
    sort key = local Sessions[] array index
```

Folders displayed:

- **Local owned**, from the viewer's `projects.json.Items[]`. Always
  shown, even when empty.
- **Peer owned**, derived from `(peer, slug)` pairs that appear in
  the visible session set. Shown when at least one session
  populates the pair. Empty peer projects do not render in v1.

A peer folder is *implicitly* defined by the wire: "at least one
visible session carries this `(peer, slug)` stamp." The wire
carries no enumeration of peer projects; viewers don't need one,
because an empty peer project has nothing to render and nothing
to sort. This avoids a second piece of state to keep in sync.

### Viewer-side folder ordering (out of scope)

The order of folders *within a single viewer's sidebar* — where
`[T] gmux` appears among the local folders — is a per-viewer
view preference, not project state. It is intentionally not
modelled in this ADR.

For the rendering rule above, a deterministic default is enough:
local folders first in the viewer's `Items[]` order, then peer
folders sorted by peer name then by origin's project order. A
later additive feature can let viewers persist a custom
`(peer, slug)` ordering in their own local config; that change
does not touch the wire and does not affect any other host. Its
state is correctly local, by the same reasoning that puts project
state on the curator host: a viewer's preferences are about that
viewer's UI.

### Local auto-assignment

The local auto-assigner runs only for sessions with `Peer == ""`.
Receivers never write peer-owned session keys to their own
`Sessions[]` arrays via the claimed path. They may write peer-owned
keys via the disclaimed-fallback path (the viewer's own match rules
adopting an origin-disclaimed session).

This contains "peer keys in the local file" to the cases where the
viewer's own intent has put them there.

### Receiver rule on incoming `snapshot.sessions` (per ADR 0001)

Per session in the new sessions snapshot, compute
`(prev_claimed, now_claimed)` relative to the previous sessions
snapshot:

| Transition | Action on viewer |
|---|---|
| `false → true` (became claimed) | `RemoveSessionFromAll(key)` from local arrays |
| `true → false` (became disclaimed) | `AutoAssignSession(s)` (idempotent) |
| `false → false` (still disclaimed) | `AutoAssignSession(s)` (idempotent; handles late slug attribution) |
| `true → true` (still claimed, possibly different slug/index) | no array mutation; render reflects new stamps |
| session removed | `RemoveSessionFromAll(key)` |

### Cross-host actions

- **Per-session actions** (kill, dismiss, attach, resume, restart):
  unchanged. Already forward to the owner via
  `peerManager.FindPeer(sessionID)`.
- **Project-level actions** (reorder a folder's `Sessions[]`):
  routed via a new generic peer proxy at `/v1/peers/{peer}/...`.
  The proxy forwards the inner request to the named peer's gmuxd
  with the auth token. The frontend selects the URL based on
  whether the folder is local-owned or peer-owned.
- **Dismiss routing matrix**:

  ```
  if s.Peer == "":             handle locally (kill if alive; remove from local array)
  elif s.Alive:                forward to origin (kill)
  elif s.ProjectSlug != "":    forward to origin (origin removes from its array)
  else:                        local removal only (viewer's array)
  ```

### Peer disconnect

When a peer is unreachable, its sessions remain cached in the
viewer's store with the data from the last received
`snapshot.sessions`. The sidebar renders them with an "unavailable"
indicator; the viewer's own `snapshot.world.peers` reflects the
disconnected status. No cleanup of viewer's `Sessions[]` keys runs
while the peer is offline.

On reconnect, the peer's first `snapshot.sessions` is authoritative.
Sessions present in the viewer's prior cache but absent from the
new sessions snapshot are removed via the receiver rule, which
cascades through `RemoveSessionFromAll` to clean any local array
entries that referenced them.

### The "+" button

Each folder has an unambiguous owner: local for own folders,
the peer for `(peer, slug)` folders. The launch button on a folder
passes `peer=<owner>` (or omits it for local). The user always
knows where a new session will run.

## Consequences

### Positive

- **"+" button is unambiguous.** No more silent host-mismatch on
  launches.
- **Cross-host reorder, dismiss, and new-session visibility work
  uniformly.** All routed via "actions go to the owner."
- **No `projects.json` synchronization across hosts.** Each host
  retains its own configuration; the sidebar reflects each host's
  view of the world.
- **Devcontainer / no-projects-on-peer case is preserved.** The
  origin disclaims; the viewer adopts via local rules; the user
  sees the same mixed-folder behaviour as today.
- **Peer disconnect is honest.** Stale data is visibly stale
  rather than silently rendered as live.
- **Match rule complexity has a clear locus.** Rules apply to a
  host's own sessions and to disclaimed peer sessions on the
  viewer; they do not pretend to span hosts.

### Negative

- **One-time UX bump.** A session whose origin newly creates a
  matching project will move out of the viewer's local folder
  into a `(peer, slug)` folder. Documented in release notes; the
  new location is more honest than the old.
- **Empty peer projects don't render on viewers in v1.** A user
  who creates `gmux` on host B with no sessions yet must launch
  the first session from B's UI (or use a generic launcher that
  picks B as the host). Addressable later with a small protocol
  addition; deferred for v1.
- **One fiddly action route.** Dismiss of a disclaimed
  dead-resumable peer session is viewer-local and does not
  forward to origin (the origin doesn't track it). The four-case
  table above encodes this; a small unit test set covers it.

### Breaking

Tied to the wire-protocol break in ADR 0001. Not separately
versioned.

## Alternatives considered

### A. Subscription mechanism

A host can ask peers to track named projects on its behalf, with
the tracked projects living as hidden entries in the peer's
`projects.json` (`subscribed_from` field, multi-entry-per-slug,
explicit lifecycle).

Rejected. The auto-discovery fallback gives us the same benefit
(peer auto-matches sessions in `~/dev/foo` even when its user has
no project for them) with one bit (`project_slug` empty) and no
new state, no new endpoint, no reconciliation.

### B. Synchronize `projects.json` across peers

Treat project state as one shared document with conflict resolution.

Rejected. Requires CRDT or similar machinery; ignores the
legitimate per-host customization (different hosts may have
different views of what counts as a project, may want different
slugs locally, etc.).

### C. Mix sessions across hosts within one project folder, with cross-host drag-reorder

A single `gmux` folder containing local + peer-A + peer-B
sessions, draggable across hosts.

Rejected. Requires complex ordering policy (whose order wins?
how do conflicts resolve?), keyspace conversion when forwarding
mixed reorder PATCHes, UX guards for impossible drops. The "+"
button location remains ambiguous. Host-scoped folders eliminate
all of this.

### D. Per-session `ProjectSlug` only, no `ProjectIndex`

Drop position from the wire; receivers sort by an intrinsic
property (created_at).

Rejected. Defeats reorder propagation entirely. Drag-reorder on
the origin would not affect viewers' sort. We want the order to
flow with the rest of the state.

### E. Explicit "headless" config flag

Add `headless = true` (or a CLI `--headless`) to mark hosts that
produce sessions but never curate projects. Gate the disclaim →
free-game cascade on this flag, so disclaimed sessions from a
non-headless host stay strictly under that host's `(peer, ...)`
in viewers' sidebars rather than being adoptable.

Rejected. The flag would be redundant with information already on
the wire: a host that doesn't curate projects has an empty
`projects.json` and so disclaims everything; a host that does
curate but has a one-off unclaimed session disclaims just that
one. Both cases want the same viewer behaviour ("if my rules
match, adopt"), and a single rule covers both. Adding a flag
would force the user to think about an extra axis of
configuration without changing any concrete behaviour they care
about.

A web-server-disable flag (security knob: don't bind a UI on a
build box) is a reasonable separate setting, but it should not
affect project semantics.

## Appendix: worked scenarios

These trace the receiver rule end-to-end through the situations
that motivated this ADR. V is a viewer; O is an origin peer.

### A. New session on origin O; O has a matching project

1. O creates session S. Local auto-assigner adds S's key to
   `O.gmux.sessions`. `Reconcile` stamps S with
   `(project_slug="gmux", project_index=N)`.
2. O emits a snapshot with the stamps.
3. V receives. Diff vs prior: S is new, claimed. No fallback
   action needed.
4. V renders S under `(O, gmux)` at index N.

### B. New session on origin O; O has no matching project

1. O creates session S. Auto-assigner finds no match. S has
   `project_slug=""`.
2. O emits a snapshot with empty stamps.
3. V receives. Diff: S is new, disclaimed. `AutoAssignSession`
   runs locally. If V's rules match, S's key appends to V's
   matching project's `Sessions[]`.
4. V renders S under V's local folder per V's array.

### C. User on O creates a project that retroactively matches existing sessions

1. `projectMgr.Update` adds the project. `AutoAssignAllAlive`
   walks own sessions, populates the new array. `Reconcile`
   stamps each affected session.
2. O emits a snapshot.
3. V receives. Diff: each affected session transitions
   `false → true`. V calls `RemoveSessionFromAll(key)` for each.
4. V re-renders: those sessions move out of V's local folder
   into `(O, gmux)`.

### D. User on O removes a project (or rule) that previously claimed sessions

1. `projectMgr.Update` removes the project. `Reconcile` clears
   stamps on affected sessions.
2. O emits a snapshot.
3. V receives. Diff: each affected session transitions
   `true → false`. V calls `AutoAssignSession(s)` for each.
4. If V's rules match, sessions enter V's local folder.
   Otherwise they fall to the unmatched / discovered UI.

### E. User on V dismisses a claimed session (origin O, alive)

1. V's gmuxd receives the dismiss action. `peerManager.FindPeer`
   routes to O.
2. O kills the runner, removes S from `O.gmux.sessions`, removes
   S from O's store.
3. O emits a snapshot. S is absent.
4. V receives. Diff: S is missing → V calls
   `RemoveSessionFromAll(key)` (idempotent — wasn't there because
   origin had claimed it).

### F. User on V dismisses a claimed dead-resumable session (origin O)

1. Forward to O. O's dismiss handler: not alive, no kill.
   Removes from `O.gmux.sessions`. `Reconcile` clears the stamp.
2. O emits a snapshot. S is still present (dead-resumable) but
   `project_slug=""`.
3. V receives. Diff: S transitions `true → false`.
   `AutoAssignSession` runs but skips dead sessions; S is not
   re-adopted.
4. S has no `project_slug` and is dead-resumable; the rendering
   rule shows it nowhere. The dismiss intent is preserved.

### G. User on V dismisses a disclaimed peer session

1. V's frontend invokes dismiss for S where `Peer=O` and
   `project_slug=""`. The four-case route matches: dead
   disclaimed → local removal only.
2. V's `projectMgr.RemoveSessionFromAll(key)` clears S from V's
   own array. No forward to O.
3. (If S was alive: the alive branch fires first, forwarding to
   kill at O. Cleanup follows via the next snapshot.)

### H. Reorder of a peer-owned folder

1. V's UI computes new order for `(O, gmux)`. Un-namespaces
   session ids to O's keyspace.
2. V's frontend PATCHes
   `/v1/peers/O/projects/gmux/sessions`. V's gmuxd forwards via
   the generic peer proxy to O's
   `PATCH /v1/projects/gmux/sessions`.
3. O writes the new order. `Reconcile` updates `project_index`
   on each affected session.
4. O emits a snapshot. V re-renders.

### I. Reorder of a local mixed folder (own + disclaimed peers)

1. V's UI computes new order. Keys are local-keyspace (slugs or
   ids).
2. V's frontend PATCHes V's own
   `/v1/projects/{slug}/sessions`. V writes its own
   `projects.json`. No broadcast to peers (this is V's local
   view of the disclaimed-fallback adoption).

### J. Origin O restarts

1. O's `sessionmeta.Sweep` reloads previously-known dead sessions
   from disk into the store (`Alive=false`). Live runners that
   are still listening register shortly after via the discovery
   scan, upserting with `Alive=true`. Peer-owned records are not
   persisted, so the store starts empty for those.
2. O loads `projects.json` and runs an initial `Reconcile` against
   the now-populated store, stamping every owned session with its
   `project_slug` / `project_index`.
3. New SSE subscriptions get a `snapshot.sessions` (and the world
   snapshot for browser consumers).
4. V's first `snapshot.sessions` from O after restart serves as the
   authoritative replay; the receiver rule reconciles V's local
   arrays. Sessions O previously had but no longer does (e.g., O's
   meta dir was wiped) are absent and trigger
   `RemoveSessionFromAll` on V.

### K. Viewer V restarts

1. V loads its own `projects.json`. Has stale entries from prior
   run (peer keys no longer applicable).
2. V's session store is empty for peer sessions until subscriptions
   land.
3. As each peer's snapshot arrives, the receiver rule applies. Stale
   peer keys whose sessions are now claimed get removed via
   `RemoveSessionFromAll`. Stale peer keys whose origin never
   reconnects sit dormant — they don't render (no resolved store
   entry) and are addressable via Manage projects.

### L. Peer O disconnects from V

1. V's `peerManager` flips O to disconnected. Cached sessions
   from O persist in V's store.
2. V's snapshot to its own browser includes O's sessions with the
   prior stamps. The browser checks `peers[O].status` and renders
   them with an "unavailable" indicator.
3. No cleanup runs while O is disconnected.
4. On reconnect, O's first snapshot is authoritative. Sessions
   absent from it trigger `RemoveSessionFromAll`. Sessions present
   refresh in place. Local `Sessions[]` cleanup follows from the
   receiver rule.
