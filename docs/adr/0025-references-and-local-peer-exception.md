# ADR 0025: Project references and the Local-peer exception

**Status:** Accepted
**Date:** 2026-05-22
**Related:** ADR 0002 (Project ownership from session origin)

> **Note (2026-07):** Renumbered from ADR 0005 → 0025 to resolve a number
> collision with "ADR 0005: CLI routes session actions through gmuxd" (two
> files claimed 0005). This document's decision and date are unchanged; it
> extends ADR 0002 and is the canonical record for the reference /
> Local-peer model. Cross-references in ADRs 0007 and 0017 were updated.

## Context

ADR 0002 established that each session's project assignment lives on
its origin host: `Reconcile` on the owning daemon stamps
`project_slug` / `project_index` onto the session, and viewers render
sidebar folders from those stamps. The ADR also kept a fallback
escape hatch: when an origin disclaims a session (`project_slug ==
""`), the viewer's own match rules adopt it into the viewer's local
folders.

Field use has shown the fallback is the dominant source of sidebar
"magic":

1. **Membership is undiscoverable.** Whether a peer session appears
   in one of the viewer's folders depends on the peer's
   `projects.json` (does it claim the session?) and the viewer's
   match rules (does any of them match the session's cwd / remote?).
   Neither host's UI shows the user how this question is being
   answered.
2. **Behaviour depends on inverse configuration.** If a viewer
   configures a rule for `~/dev/foo`, that rule will only adopt peer
   sessions whose `cwd` happens to be exactly that path. In typical
   devcontainer setups (`/workspaces/foo` inside the container), it
   won't, and the user sees nothing. In `localWorkspaceFolder`
   setups, it does, but the user can't tell why.
3. **There is no preview.** When adding a project, the viewer can't
   show "here is what would land in this folder" without enumerating
   sessions across every host the rule might match. The result is
   either misleading (local-only) or wrong (rule semantics differ
   from server-side).
4. **Peer folders are emergent, not configured.** A peer folder
   appears when stamped sessions arrive and vanishes when the last
   one closes. The user has no way to pin, hide, or preview one.

The fix is to make the cross-host story explicit instead of
emergent. This ADR specifies that change.

## Decision

### Each project belongs to exactly one host

Project ownership is **strict per-host**. A session's project
assignment is determined solely by the rules in its origin's
`projects.json`. Viewer match rules never adopt sessions from
elsewhere.

### The viewer's `projects.json` items[] holds two kinds of entry

- **Owned** (existing shape): `{slug, match[], sessions[]}`. The
  viewer's own project. `match[]` drives session attribution by the
  viewer's `Reconcile`; `sessions[]` is the ordered session-key
  list.
- **Reference** (new): `{slug, peer}`. A pointer to a peer-owned
  project. The peer's `projects.json` is the source of truth for
  match rules and session order. The viewer just declares "show
  this peer's project in my sidebar at this position."

The sidebar order is items[] order, mixing freely. Validation
enforces exactly one of `{match[] present, peer present}` per
entry. Owned-and-reference can coexist with the same slug (two
distinct folders, disambiguated by `(peer, slug)`).

Schema version bumps to 3. v2 → v3 is a pass-through; `MatchRule.Hosts`
is silently dropped on load (scoping is implicit in ownership now).

### Wire format

`snapshot.world` gains `peer_projects: { [peer]: [{slug, launch_cwd?}] }`
and `PeerInfo.local: bool`. The hub fetches each peer's `GET
/v1/projects` on connect and on `projects-update` (now forwarded to
`?as=peer` consumers too), caches a minimal projection, and
re-broadcasts it in its own `snapshot.world`. The viewer uses this to
enumerate references in the Manage Projects modal and to fill
`launchCwd` for empty referenced folders.

### Hub proxy

The existing `PATCH /v1/peers/{peer}/v1/projects/{slug}/sessions`
proxy is joined by `POST /v1/peers/{peer}/v1/projects/add`. The
Manage Projects modal uses the latter to create a project on a peer
from the Discovered section.

### The Local-peer exception

Devcontainers don't have user-curated `projects.json`. They report
container-side paths (`/workspaces/foo`) that don't match the host's
project rules. Asking the user to configure projects inside every
container — and then reference them from the host — is significant
friction for the most common gmux usage pattern after plain-local.

A `Local` peer (`PeerConfig.Local = true`, set only by the Docker
watcher) is conceptually an extension of the parent host:

- Same user, same machine, ephemeral lifecycle.
- The parent's `Reconcile` runs match rules against Local-peer
  sessions and writes the resulting slug/index into the session as
  it flows through the parent's snapshot. The container's own
  `projects.json`, if any, is ignored from the parent's perspective.
- Local-peer session keys land in the parent's
  `projects.json.Items[].Sessions[]` (namespaced ids for slugless
  sessions; bare slugs for attributed ones). On container removal,
  `peering.Manager.OnPeerRemoved` fires
  `projects.Manager.PruneNamespacedKeys(name)` to drop accumulated
  `@<peer>` keys from owned items.
- The sidebar buckets Local-peer sessions into the parent's local
  folder, not a peer folder. The session row still renders the peer
  chip so the user knows it's a container session.

The exception is **narrow and named**: gated on a single flag, set
in exactly one place. Network peers remain strict per ADR 0002.

### `Reconcile` becomes caller-driven

`store.Reconcile`'s assignFn is now called for every session; the
caller decides per-session whether to preserve the existing stamp
(network peers) or compute a fresh one (local or Local peers). The
canonical caller in `main.go` encodes this rule:

```go
sessions.Reconcile(func(s store.Session) (string, int) {
    if s.Peer != "" && !peerManager.IsLocalPeer(s.Peer) {
        return s.ProjectSlug, s.ProjectIndex // network peer: preserve
    }
    a := assignments[s.ID]
    return a.Slug, a.Index
})
```

### Resumable sessions preserve existing membership only

Auto-assign (per-event and startup-sweep) covers live sessions only.
Dead/resumable runtime state lives in sessionmeta, while sidebar
membership lives in `projects.json`. If `Sessions[]` already contains
a dead session's key, reconcile can still stamp it and keep it visible;
if dismiss removed the key, a late exit event or startup sweep must not
add it back as resumable. This keeps stamps as the sole authority for
rendering without letting runtime discovery override explicit sidebar
membership.

## Consequences

### Wins

- Sidebar membership is determined entirely by stamps. One model,
  one source of truth. No client-side fallback paths.
- Adding a project gets a meaningful preview: run match rules
  against the target host's sessions, see exactly what would land.
- Remote-ness is intrinsic to a project (owned vs reference),
  visualized via the peer chip on the folder header.
- Empty references render and pin so users can launch into them,
  even when the peer is reachable but the project is currently
  inactive.
- Peer folders survive across reconnect / sleep cycles (today's
  emergent peer folders vanish when sessions die).

### Costs

- **Cross-host adoption removed.** Users who previously got
  devcontainer or v1-spoke sessions adopted into local folders via
  matching cwds need to either configure a project on the peer
  (typical case after this ADR) or accept that disclaimed peer
  sessions surface only via the Manage Projects modal's Discovered
  section.
- **Same-repo-on-many-hosts is N folders, not one.** If the user
  references a peer's `gmux` from N hosts, the sidebar has N
  folders for the same logical repo. The "all sessions" tab
  (deferred) addresses the many-tiny-hosts case where the
  per-host decomposition feels excessive.
- **`MatchRule.Hosts` is gone.** Anyone who used it (probably
  nobody) gets a one-time silent migration via the v3 bump; the
  field is dropped from v2 files on next save.

### Non-goals

- **Path translation.** Devcontainer mounts often map host paths
  to different in-container paths. The Local-peer exception only
  partly fixes this: stamps use the parent's rules, which see the
  parent's paths on the local sessions but the *container's* paths
  on Local-peer sessions. Path translation (container reports
  host-equivalent paths in `workspace_root`) is a separate concern,
  tracked as a follow-up. Git-remote matching covers the common
  case in the meantime.
- **All-sessions tab.** Surfacing every session across every host
  with configurable grouping is a separate epic.
- **Cross-host project reorder UX.** Each peer-referenced folder's
  session order is the peer's. The viewer can reorder via PATCH
  proxy, which is global (every viewer sees the new order). There
  is intentionally no per-viewer override.

## Implementation notes

- The change is forward-incompatible with v1.x daemons reading v3
  `projects.json`: older daemons mis-parse reference items. This is
  acceptable because the relevant feature surface (cross-host
  ownership, snapshot push, namespaced session keys) was added in
  the v1.x cycle and isn't expected to round-trip cleanly.
- Disclaimed sessions on disconnected peers are excluded from the
  unmatched-active badge. Their disclaimed state may be stale; the
  count would only nag users about peers they can't act on anyway.
- Dangling references (peer is connected but reports no such slug)
  render with a `?` badge. The user removes them manually via
  Manage Projects. No auto-prune: that's the kind of magic this
  ADR is moving away from.
