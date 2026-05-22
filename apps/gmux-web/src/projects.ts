// --- Project-session matching and topology ---
//
// Maps sessions to projects using match rules (path prefix, git remote).
// Builds sidebar folders and project hub topology. Pure functions with
// no side effects or signal dependencies.

import type { Session, Folder, ProjectItem, PeerInfo, DiscoveredProject } from './types'

// --- Remote normalization (mirrors Go NormalizeRemote) ---

export function normalizeRemote(url: string): string {
  for (const prefix of ['https://', 'http://', 'ssh://', 'git://']) {
    if (url.startsWith(prefix)) { url = url.slice(prefix.length); break }
  }
  const at = url.indexOf('@')
  if (at >= 0) url = url.slice(at + 1)
  const colon = url.indexOf(':')
  if (colon > 0 && !url.slice(0, colon).includes('/')) {
    url = url.slice(0, colon) + '/' + url.slice(colon + 1)
  }
  return url.replace(/\.git$/, '').replace(/\/+$/, '')
}

// --- Matching ---

function pathUnder(candidate: string | undefined, base: string): boolean {
  if (!candidate || !base) return false
  if (candidate === base) return true
  return candidate.startsWith(base + '/')
}

/**
 * Whether a session should be visible in this project's UI (sidebar
 * folder, project hub page).
 *
 * Under the references model, stamps are the sole authority for folder
 * membership. A session arrives in a folder because its origin host
 * stamped it; we then just decide whether to render it based on
 * liveness. Alive and resumable sessions render; dead non-resumable
 * sessions are hidden (no terminal clutter from one-shot commands).
 *
 * The `project` argument is retained for call-site symmetry but the
 * check no longer reads project.sessions[] — stamps replace that
 * indirection.
 */
export function isSessionVisibleInProject(session: Session, _project: ProjectItem): boolean {
  if (session.alive) return true
  return session.resumable === true
}

/**
 * Returns the project that best matches a session, or null.
 *
 * Mirrors Go State.Match: checks each project's match rules.
 * Path rules use longest-prefix matching. If no path rule matches,
 * falls back to the first remote-matched project.
 *
 * Both project paths and session cwds are canonicalized server-side
 * (~/... form), so string comparison works without $HOME expansion.
 * Does not check rule.hosts (host scoping is server-side only).
 */
export function matchSession(
  session: Session,
  projects: ProjectItem[],
): ProjectItem | null {
  let bestPath: ProjectItem | null = null
  let bestPathLen = 0
  let firstRemote: ProjectItem | null = null

  for (const project of projects) {
    // References don't carry local match rules; their content is
    // driven by peer stamps, not viewer-side matching.
    if (project.peer) continue
    for (const rule of project.match ?? []) {
      if (rule.remote && session.remotes) {
        const normRule = normalizeRemote(rule.remote)
        for (const url of Object.values(session.remotes)) {
          if (normalizeRemote(url) === normRule) {
            if (!firstRemote) firstRemote = project
            break
          }
        }
      }

      if (rule.path) {
        const matched = rule.exact
          ? (session.cwd === rule.path || session.workspace_root === rule.path)
          : (pathUnder(session.cwd, rule.path) || pathUnder(session.workspace_root, rule.path))
        if (matched && rule.path.length > bestPathLen) {
          bestPathLen = rule.path.length
          bestPath = project
        }
      }
    }
  }

  return bestPath ?? firstRemote
}

// --- Slug helpers (mirror Go projects.Slugify, SlugFromRemote, SlugFromPath) ---

/**
 * Convert a string to a URL-safe slug. Mirrors Go projects.Slugify:
 * lowercases, maps non-alnum to '-', collapses runs of '-', trims '-'
 * from both ends. Returns 'project' for empty results.
 */
export function slugify(s: string): string {
  s = s.toLowerCase()
  let out = ''
  for (const ch of s) {
    if ((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) out += ch
    else out += '-'
  }
  while (out.includes('--')) out = out.replaceAll('--', '-')
  out = out.replace(/^-+|-+$/g, '')
  return out || 'project'
}

/** Derive a slug from a git remote URL by slugifying the repo name
 * (last segment of the normalized URL). */
export function slugFromRemote(remote: string): string {
  const norm = normalizeRemote(remote)
  const parts = norm.split('/')
  return slugify(parts[parts.length - 1] ?? '')
}

/** Derive a slug from a filesystem path by slugifying the basename.
 * Cwd values reaching the frontend are already canonicalized server-side,
 * so a simple last-segment extraction matches the Go SlugFromPath behaviour. */
export function slugFromPath(p: string): string {
  const trimmed = p.replace(/\/+$/, '')
  const idx = trimmed.lastIndexOf('/')
  const base = idx >= 0 ? trimmed.slice(idx + 1) : trimmed
  return slugify(base)
}

// --- Discovered projects + unmatched active count ---
//
// These were previously computed server-side (projects.State.Discovered
// and UnmatchedActiveCount). Per ADR 0001 they're per-viewer concerns:
// each frontend computes them from its merged sessions + projects view
// rather than the server pushing them in the snapshot. Pure functions
// here; the store wires them up as `computed()` projections.

/** Most frequently appearing normalized remote URL across the given
 * sessions, or '' if none have remotes. Tie-break: lexicographically
 * earliest URL wins (matches Go mostCommonRemote). */
export function mostCommonRemote(sessions: Session[]): string {
  const counts = new Map<string, number>()
  for (const s of sessions) {
    if (!s.remotes) continue
    for (const url of Object.values(s.remotes)) {
      const norm = normalizeRemote(url)
      counts.set(norm, (counts.get(norm) ?? 0) + 1)
    }
  }
  let best = ''
  let bestN = 0
  for (const [url, n] of counts.entries()) {
    if (n > bestN || (n === bestN && url < best)) {
      best = url
      bestN = n
    }
  }
  return best
}

/** Group disclaimed sessions into suggested projects, scoped per host.
 *
 * Each host's discovery is independent: a session is in scope for host X
 * iff (s.peer ?? '') === X and s.project_slug is empty. The viewer no
 * longer adopts peer sessions via its own match rules, so the bucket is
 * exactly "sessions whose origin host hasn't filed them."
 *
 * Suggestions from different hosts mix into one returned list, sorted
 * by recency (most-recently-active first). Each carries a `peer` field
 * (absent for local) so the modal can render a host chip and route the
 * "+ Add" action to the right daemon.
 *
 * Local groups whose disclaimed sessions would still match a local
 * owned project are skipped: the user already has a place for them, and
 * the auto-assign hook will stamp them on next attribution. */
export function discoverProjects(
  sessions: Session[],
  projects: ProjectItem[],
): DiscoveredProject[] {
  // Bucket by (peer, dir). peer '' is local; dir is workspace_root if
  // set, else cwd. Sessions with no dir are dropped.
  const byKey = new Map<string, { peer: string; dir: string; group: Session[] }>()
  for (const s of sessions) {
    if (s.project_slug) continue // claimed by origin
    const peer = s.peer ?? ''
    if (peer === '') {
      // Local-host discovery still defers to local owned projects:
      // a session matching a local rule will be stamped imminently by
      // auto-assign, so don't surface it as discovered.
      if (matchSession(s, projects)) continue
    }
    const dir = s.workspace_root || s.cwd
    if (!dir) continue
    const key = `${peer}\u0000${dir}`
    let bucket = byKey.get(key)
    if (!bucket) { bucket = { peer, dir, group: [] }; byKey.set(key, bucket) }
    bucket.group.push(s)
  }
  if (byKey.size === 0) return []

  const result: DiscoveredProject[] = []
  for (const { peer, dir, group } of byKey.values()) {
    const active = group.filter(s => s.alive).length
    const remote = mostCommonRemote(group)
    let suggested = remote ? slugFromRemote(remote) : ''
    if (!suggested) suggested = slugFromPath(dir)
    if (!suggested) suggested = 'project'
    let lastActive = ''
    for (const s of group) {
      if (s.created_at > lastActive) lastActive = s.created_at
    }
    const dp: DiscoveredProject = {
      suggested_slug: suggested,
      paths: [dir],
      session_count: group.length,
      active_count: active,
    }
    if (remote) dp.remote = remote
    if (peer) dp.peer = peer
    if (lastActive) dp.last_active = lastActive
    result.push(dp)
  }

  // Sort by recency, then active count, then session count, then
  // suggested_slug, then by the directory path that originated this
  // discovered project.
  //
  // The final paths[0] tiebreak matters: two sessions whose cwds
  // have the same basename (e.g. `/home/me/api` and `/srv/api`)
  // bucket into distinct discovered projects but produce identical
  // suggested_slug values via slugFromPath. Without it, the sort
  // falls through to the input order, which is the byKey Map's
  // insertion order, which mirrors the (Go-map-randomized)
  // snapshot.sessions order — so the two rows flip on every
  // snapshot re-emit. paths[0] is unique per discovered project
  // because it is the very key used to build the bucket.
  result.sort((a, b) => {
    const ta = a.last_active ?? ''
    const tb = b.last_active ?? ''
    if (ta !== tb) return tb < ta ? -1 : 1
    if (a.active_count !== b.active_count) return b.active_count - a.active_count
    if (a.session_count !== b.session_count) return b.session_count - a.session_count
    const slugCmp = a.suggested_slug.localeCompare(b.suggested_slug)
    if (slugCmp !== 0) return slugCmp
    return a.paths[0].localeCompare(b.paths[0])
  })
  return result
}

/** Number of alive sessions outside any project, summed across every
 *  connected host. Drives the "N active sessions outside any project"
 *  badge.
 *
 *  Under the references model (ADR 0002 + amendment), "outside any
 *  project" is a per-host property: a session is unmatched iff its
 *  origin host disclaims it (`project_slug == ""`). Viewer match rules
 *  no longer adopt peer sessions, so this count is the union of every
 *  host's disclaimed-alive sessions.
 *
 *  Sessions on disconnected peers are excluded: their disclaimed
 *  status could be stale (peer might have a project rule that adopts
 *  them once reachable), and badging the user about an unreachable
 *  peer's discovery is noise. */
export function countUnmatchedActive(
  sessions: Session[],
  _projects: ProjectItem[],
  peerStatusByName?: ReadonlyMap<string, string>,
): number {
  let count = 0
  for (const s of sessions) {
    if (!s.alive) continue
    if (s.project_slug) continue // claimed by origin
    if (s.peer && peerStatusByName) {
      const status = peerStatusByName.get(s.peer)
      if (status !== 'connected') continue
    }
    count++
  }
  return count
}

// --- Sidebar folders ---

/**
 * Build the sidebar folder list.
 *
 * Each entry in the viewer's `projects` items[] becomes one folder,
 * in items[] order (user-controlled). Two kinds of entry:
 *
 *   - **Owned** (`peer` absent): folder is filled by sessions stamped
 *     with this slug whose peer matches (local sessions for a local
 *     owner, plus Local-peer sessions whose stamps the parent applies).
 *   - **Reference** (`peer` set): folder is filled by sessions stamped
 *     with this slug AND originating from the named peer.
 *
 * Sessions route purely by stamps. There is no client-side fallback to
 * viewer match rules: matching happens only on the owning host. A
 * session whose origin disclaims it is never adopted by the viewer; it
 * surfaces via `discoverProjects` / `countUnmatchedActive` only.
 *
 * Empty folders still render: the entry is in projects.json by user
 * intent, the empty state is informative ("No sessions on workstation
 * right now"), and references in particular need to remain pinned so
 * the user can launch into them.
 *
 * Local-peer sessions (devcontainers; `peers[s.peer].local === true`)
 * are bucketed as if local: their stamps come from the parent's match
 * rules, and they live in the parent's folder. The peer chip still
 * renders on the session row so the user knows it's a container
 * session.
 */
export function buildProjectFolders(
  projects: ProjectItem[],
  sessions: Session[],
  isLocalPeer?: (peerName: string) => boolean,
): Folder[] {
  // Bucket every stamped session by `${ownerPeer}::${slug}`.
  // ownerPeer is '' for sessions owned by the viewer (local sessions,
  // and Local-peer sessions whose project ownership lives on the
  // parent), else the originating peer's name.
  const buckets = new Map<string, Session[]>()
  const bucket = (ownerPeer: string, slug: string, s: Session): void => {
    const key = `${ownerPeer}::${slug}`
    let arr = buckets.get(key)
    if (!arr) { arr = []; buckets.set(key, arr) }
    arr.push(s)
  }

  for (const s of sessions) {
    if (!s.project_slug) continue // unstamped: surfaces via discovery only
    const sessionPeer = s.peer ?? ''
    const ownerPeer = sessionPeer && !(isLocalPeer?.(sessionPeer))
      ? sessionPeer
      : ''
    bucket(ownerPeer, s.project_slug, s)
  }

  const folders: Folder[] = []
  for (const project of projects) {
    const ownerPeer = project.peer ?? ''
    const ss = buckets.get(`${ownerPeer}::${project.slug}`) ?? []
    const visible = ss.filter(s => s.alive || s.resumable === true)
    visible.sort(compareFolderSessions)
    folders.push({
      key: `${ownerPeer}::${project.slug}`,
      slug: project.slug,
      name: project.slug,
      peer: ownerPeer || undefined,
      launchCwd: project.match?.find(r => r.path)?.path,
      sessions: visible,
    })
  }

  return folders
}

/**
 * Sort key for a session inside a folder.
 *
 * Stamps are now the sole authority for both folder membership and
 * ordering: every session that lands in a folder is stamped, and its
 * `project_index` reflects the owning host's authoritative position
 * (the index in projects.json `Sessions[]`). Ties are unlikely (the
 * server hands out distinct indices) but we fall back to `created_at`
 * then `id` so the order is deterministic across snapshot re-emits.
 */
function compareFolderSessions(a: Session, b: Session): number {
  const idx = (a.project_index ?? 0) - (b.project_index ?? 0)
  if (idx !== 0) return idx
  const dt = new Date(a.created_at).getTime() - new Date(b.created_at).getTime()
  if (dt !== 0) return dt
  return a.id.localeCompare(b.id)
}

// --- Project hub topology ---

/** Sessions in a single working directory on a host. */
export interface FolderNode {
  cwd: string
  sessions: Session[]
}

/** A host (local or peer) with all its project sessions, grouped by cwd. */
export interface HostNode {
  /**
   * Path from the outermost peer down to this host (root-first). Empty
   * array for the local gmuxd. E.g. `['workstation']` for a direct peer,
   * `['workstation', 'alpine-dev']` for a devcontainer nested on that peer.
   */
  path: string[]

  /**
   * Connection state. Local hosts report 'local'; direct peers mirror
   * `/v1/peers`. Nested hosts inherit their root peer's status since the
   * hub only sees the outermost hop directly.
   */
  status: 'local' | 'connected' | 'connecting' | 'disconnected'

  /** Free-form display hint (peer URL for remote, 'local' for local). */
  meta: string

  /** Folders grouped by cwd, sorted alphabetically. */
  folders: FolderNode[]
}

/**
 * Parse a (possibly namespaced) session ID into its original identity and
 * host path.
 *
 *   "sess-abc"            -> { originalId: "sess-abc", path: [] }
 *   "sess-abc@spoke"      -> { originalId: "sess-abc", path: ["spoke"] }
 *   "sess-abc@dev@spoke"  -> { originalId: "sess-abc", path: ["spoke", "dev"] }
 *
 * The `@` chain is innermost-first (peering.NamespaceID *appends* when
 * propagating up), so reversing gives the outermost-first path a human
 * reads root -> leaf.
 */
export function parseSessionHostPath(sessionId: string): { originalId: string; path: string[] } {
  const parts = sessionId.split('@')
  const [originalId, ...chain] = parts
  return { originalId, path: chain.reverse() }
}

/**
 * Build the keys to send to `PATCH /v1/projects/{slug}/sessions` (or
 * its peer-proxy equivalent) given the order the viewer just dragged
 * a folder into. Two responsibilities:
 *
 *  1. Filter out sessions not owned by the folder's owner. A local
 *     folder may visually contain peer-owned sessions adopted via
 *     match rules; those don't live in the local projects.json, and
 *     sending them would add phantom entries on the daemon's next
 *     ReorderSessions merge.
 *
 *  2. Key sessions appropriately for the owning daemon's projects.json:
 *     - For references (folder.peer set, not a Local peer): the peer's
 *       projects.json keys by the original (unnamespaced) id, so we
 *       strip `@<peer>` from slugless ids before sending.
 *     - For local folders (folder.peer absent or Local): the parent's
 *       projects.json keys may include namespaced ids for Local-peer
 *       sessions, since the parent owns project assignment for them.
 *       We keep `@<peer>` for those sessions and strip nothing for
 *       genuinely local sessions.
 *     Slugged sessions always key by `s.slug`, which is never namespaced.
 *
 * Returns an empty array when no session in the request belongs to
 * the folder owner: caller should skip the PATCH entirely so the
 * daemon doesn't see an empty reorder.
 */
export function reorderKeysForFolder(
  reorderedSessions: Session[],
  folderPeer: string | undefined,
  isLocalPeer?: (peerName: string) => boolean,
): string[] {
  const isLocalFolder = !folderPeer
  return reorderedSessions
    .filter(s => {
      const sessionPeer = s.peer ?? ''
      if (isLocalFolder) {
        // Local folder owns local sessions plus Local-peer sessions.
        return sessionPeer === '' || !!isLocalPeer?.(sessionPeer)
      }
      // Reference folder: only the peer's own sessions belong.
      return sessionPeer === folderPeer
    })
    .map(s => {
      if (s.slug) return s.slug
      const sessionPeer = s.peer ?? ''
      // For Local-peer sessions in a local folder, the parent keys by
      // the namespaced id since the namespace is part of the session's
      // identity from the parent's POV.
      if (isLocalFolder && sessionPeer !== '' && isLocalPeer?.(sessionPeer)) {
        return s.id
      }
      return parseSessionHostPath(s.id).originalId
    })
}

/**
 * Build the host topology for a single project. Sessions that match the
 * project are bucketed by their host path (derived from the session id
 * chain), then by cwd within each host. Used by the project hub page.
 *
 * Returns an empty array when the project slug is unknown or has no
 * sessions. The caller can render a project-is-empty state for that case.
 */
export function buildProjectTopology(
  projectSlug: string,
  sessions: Session[],
  projects: ProjectItem[],
  peers: PeerInfo[],
  projectPeer?: string,
  isLocalPeer?: (peerName: string) => boolean,
): HostNode[] {
  // Find the matching items[] entry. The hub can be reached at
  // `/<slug>` (local owned) or `/@<peer>/<slug>` (reference); the
  // caller passes `projectPeer` for the latter.
  const ownerPeer = projectPeer ?? ''
  const project = projects.find(p =>
    p.slug === projectSlug && (p.peer ?? '') === ownerPeer,
  )
  if (!project) return []

  // Match the sidebar bucketing: a session belongs in this project's
  // hub iff its stamp matches AND its effective owner matches the
  // project's owner. For owned projects, the owner is the viewer
  // (Local-peer sessions count as owned by the viewer because their
  // stamps come from the parent's rules). For references, the owner
  // is the named peer.
  const projectSessions = sessions.filter(s => {
    if (s.project_slug !== projectSlug) return false
    const sessionPeer = s.peer ?? ''
    const effectiveOwner = sessionPeer && !(isLocalPeer?.(sessionPeer))
      ? sessionPeer
      : ''
    if (effectiveOwner !== ownerPeer) return false
    return isSessionVisibleInProject(s, project)
  })

  // Bucket by host path. The session id chain encodes the namespace
  // hops innermost-first; parseSessionHostPath reverses them so the
  // path reads root → leaf. We strip any leading Local-peer hop:
  // those peers are co-tenants of the viewer, so their sessions live
  // in the viewer's local host node, not a separate peer node.
  const hostBuckets = new Map<string, { path: string[]; sessions: Session[] }>()
  for (const s of projectSessions) {
    const { path } = parseSessionHostPath(s.id)
    const effectivePath = path.length > 0 && isLocalPeer?.(path[0])
      ? path.slice(1)
      : path
    const key = effectivePath.join('\0')
    let bucket = hostBuckets.get(key)
    if (!bucket) {
      bucket = { path: effectivePath, sessions: [] }
      hostBuckets.set(key, bucket)
    }
    bucket.sessions.push(s)
  }

  // Convert each bucket -> HostNode with cwd-grouped folders.
  const hosts: HostNode[] = []
  for (const bucket of hostBuckets.values()) {
    const folderMap = new Map<string, Session[]>()
    for (const s of bucket.sessions) {
      const cwd = s.cwd || ''
      let list = folderMap.get(cwd)
      if (!list) { list = []; folderMap.set(cwd, list) }
      list.push(s)
    }
    const folders: FolderNode[] = [...folderMap.entries()]
      .map(([cwd, ss]) => ({ cwd, sessions: sortHubSessions(ss) }))
      .sort((a, b) => a.cwd.localeCompare(b.cwd))

    const { status, meta } = resolveHostStatusAndMeta(bucket.path, peers)
    hosts.push({ path: bucket.path, status, meta, folders })
  }

  // Local first, then peers alphabetically by full path.
  hosts.sort((a, b) => {
    if (a.path.length === 0 && b.path.length > 0) return -1
    if (b.path.length === 0 && a.path.length > 0) return 1
    return a.path.join('/').localeCompare(b.path.join('/'))
  })

  return hosts
}

function resolveHostStatusAndMeta(
  path: string[],
  peers: PeerInfo[],
): { status: HostNode['status']; meta: string } {
  if (path.length === 0) return { status: 'local', meta: 'local' }
  const rootPeerName = path[0]
  const peer = peers.find(p => p.name === rootPeerName)
  if (!peer) return { status: 'disconnected', meta: '' }
  const raw = peer.status
  const status: HostNode['status']
    = raw === 'connected' || raw === 'connecting' || raw === 'disconnected'
      ? raw
      : 'disconnected'
  return { status, meta: peer.url }
}

function sortHubSessions(sessions: Session[]): Session[] {
  // Alive first, then newest-first. Tiebreak on id so sessions that
  // share a second-precision created_at (e.g. v1-spoke entries
  // rehydrated together on startup) keep a stable order across
  // snapshot.sessions re-emits.
  return [...sessions].sort((a, b) => {
    if (a.alive !== b.alive) return a.alive ? -1 : 1
    const ta = new Date(a.created_at || 0).getTime()
    const tb = new Date(b.created_at || 0).getTime()
    if (ta !== tb) return tb - ta
    return a.id.localeCompare(b.id)
  })
}
