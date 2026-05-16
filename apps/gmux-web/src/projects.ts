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
 * Alive sessions always show. Dead sessions only appear if they are
 * resumable *and* listed in the project's `sessions` array, which the
 * server maintains as the set of sessions the user has touched in the
 * context of that project. Non-tracked dead sessions stay hidden so
 * the UI doesn't accumulate terminal clutter.
 */
export function isSessionVisibleInProject(session: Session, project: ProjectItem): boolean {
  if (session.alive) return true
  if (!session.resumable) return false
  const keys = new Set(project.sessions ?? [])
  const key = session.slug || session.id
  return keys.has(key) || keys.has(session.id)
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
    for (const rule of project.match) {
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

/** Group sessions that aren't claimed by any project, bucketed by
 * directory (workspace_root if set, else cwd). Each bucket becomes one
 * suggested project.
 *
 * Per ADR 0002, claimed sessions (`project_slug != ""`) have a definite
 * home on their origin host; only disclaimed sessions are eligible for
 * discovery, and only those the viewer's own rules don't already adopt.
 * That makes "discovered" exactly the set of sessions the user might
 * reasonably want to file into a new local project. */
export function discoverProjects(
  sessions: Session[],
  projects: ProjectItem[],
): DiscoveredProject[] {
  const byDir = new Map<string, Session[]>()
  for (const s of sessions) {
    if (s.project_slug) continue            // claimed by origin: not free game
    if (matchSession(s, projects)) continue  // already adopted by viewer's rules
    const dir = s.workspace_root || s.cwd
    if (!dir) continue
    let bucket = byDir.get(dir)
    if (!bucket) { bucket = []; byDir.set(dir, bucket) }
    bucket.push(s)
  }
  if (byDir.size === 0) return []

  const result: DiscoveredProject[] = []
  for (const [dir, group] of byDir.entries()) {
    const active = group.filter(s => s.alive).length
    const remote = mostCommonRemote(group)
    let suggested = remote ? slugFromRemote(remote) : ''
    if (!suggested) suggested = slugFromPath(dir)
    if (!suggested) suggested = 'project'
    const dp: DiscoveredProject = {
      suggested_slug: suggested,
      paths: [dir],
      session_count: group.length,
      active_count: active,
    }
    if (remote) dp.remote = remote
    result.push(dp)
  }

  // Active first, then most sessions, then alphabetically.
  result.sort((a, b) => {
    if (a.active_count !== b.active_count) return b.active_count - a.active_count
    if (a.session_count !== b.session_count) return b.session_count - a.session_count
    return a.suggested_slug < b.suggested_slug ? -1 : a.suggested_slug > b.suggested_slug ? 1 : 0
  })
  return result
}

/** Number of alive sessions outside any project, in the viewer's view.
 * Drives the "N active sessions outside any project" badge.
 *
 * Per ADR 0002, a session is "outside any project" iff its origin
 * disclaims it (`project_slug == ""`) AND the viewer's own match rules
 * don't adopt it. Claimed peer sessions live in the peer's folder;
 * claimed local sessions live in the viewer's folder; neither counts. */
export function countUnmatchedActive(
  sessions: Session[],
  projects: ProjectItem[],
): number {
  let count = 0
  for (const s of sessions) {
    if (!s.alive) continue
    if (s.project_slug) continue           // claimed by origin
    if (matchSession(s, projects)) continue // adopted by viewer
    count++
  }
  return count
}

// --- Sidebar folders ---

/**
 * Build the sidebar folder list per ADR 0002.
 *
 * Two kinds of folder come out of this:
 *
 *   1. **Local folders**: one per entry in the viewer's `projects` list,
 *      in `Items[]` order (user-controlled). Even an empty local folder
 *      renders, so the user always sees their own configuration.
 *   2. **Peer folders**: derived implicitly from `(peer, project_slug)`
 *      pairs that appear on stamped sessions. A peer folder exists iff
 *      at least one visible session carries that pair. Sorted
 *      deterministically by peer name then slug. Two hosts that share
 *      a slug get two distinct folders.
 *
 * Each session is routed to one folder (or none) by the rule:
 *
 *   - If `s.project_slug != ""` (claimed by origin):
 *       bucket into folder `(s.peer, s.project_slug)`.
 *       Sort key is `s.project_index` (origin's authoritative position).
 *   - Else (disclaimed): run the viewer's `matchSession` against its
 *     own projects. If matched, the session is *adopted* into that
 *     local folder; sort key is `created_at`. If unmatched, the
 *     session is visible only via `discoverProjects` /
 *     `countUnmatchedActive`.
 *
 * Visibility filter: alive sessions always render; dead sessions render
 * only when claimed by the folder they'd be in. Disclaimed-adopted dead
 * sessions are hidden (the viewer's intent didn't put them in any array;
 * the rule is the only thread holding them, and a dead session can't
 * acquire new context to validate the match).
 */
export function buildProjectFolders(
  projects: ProjectItem[],
  sessions: Session[],
): Folder[] {
  // Bucket every session by `${peer ?? ''}::${slug}`. An empty peer
  // segment marks a viewer-owned (local) bucket.
  const buckets = new Map<string, Session[]>()
  const bucket = (peer: string, slug: string, s: Session): void => {
    const key = `${peer}::${slug}`
    let arr = buckets.get(key)
    if (!arr) { arr = []; buckets.set(key, arr) }
    arr.push(s)
  }

  for (const s of sessions) {
    if (s.project_slug) {
      // Claimed by origin. Bucket under (peer, slug); peer may be
      // empty (local-claimed: viewer is the origin).
      bucket(s.peer ?? '', s.project_slug, s)
    } else {
      // Disclaimed: free-game adoption by the viewer's rules.
      const matched = matchSession(s, projects)
      if (matched) bucket('', matched.slug, s)
      // else: not in any folder; surfaces via discoverProjects.
    }
  }

  const folders: Folder[] = []

  // 1. Local folders, in viewer's Items[] order. Always emitted, even
  //    when empty: the user's own configuration is always visible.
  for (const project of projects) {
    const ss = buckets.get(`::${project.slug}`) ?? []
    const visible = ss.filter(
      s => s.alive || (s.resumable === true && s.project_slug === project.slug),
    )
    visible.sort(compareLocalFolderSessions)
    folders.push({
      key: project.slug,
      slug: project.slug,
      name: project.slug,
      launchCwd: project.match.find(r => r.path)?.path,
      sessions: visible,
    })
  }

  // 2. Peer folders, by (peer, slug). Empty peer folders are skipped
  //    — nothing on the wire enumerates peer projects, so an empty
  //    folder would be a viewer fiction.
  const peerFolders: Folder[] = []
  for (const [key, ss] of buckets) {
    if (key.startsWith('::')) continue // local: handled above
    const sep = key.indexOf('::')
    const peer = key.slice(0, sep)
    const slug = key.slice(sep + 2)
    const visible = ss.filter(s => s.alive || s.resumable === true)
    if (visible.length === 0) continue
    visible.sort((a, b) => (a.project_index ?? 0) - (b.project_index ?? 0))
    peerFolders.push({
      key,
      slug,
      name: slug,
      peer,
      sessions: visible,
    })
  }
  peerFolders.sort((a, b) => {
    const peerCmp = (a.peer ?? '').localeCompare(b.peer ?? '')
    return peerCmp !== 0 ? peerCmp : a.slug.localeCompare(b.slug)
  })
  folders.push(...peerFolders)

  return folders
}

/**
 * Sort key for a session inside a *local* folder.
 *
 * Stamped (claimed-local) sessions sort first by `project_index` — the
 * origin's authoritative position, equivalent to the index in
 * `project.sessions[]`. Unstamped sessions (disclaimed-adopted via
 * the viewer's match rules) come after, ordered by creation time so
 * newly-spawned sessions don't reshuffle existing ones.
 */
function compareLocalFolderSessions(a: Session, b: Session): number {
  const aStamped = !!a.project_slug
  const bStamped = !!b.project_slug
  if (aStamped && bStamped) {
    return (a.project_index ?? 0) - (b.project_index ?? 0)
  }
  if (aStamped) return -1
  if (bStamped) return 1
  return new Date(a.created_at).getTime() - new Date(b.created_at).getTime()
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
 *  2. Strip the `@<peer>` namespace from slugless ids. A peer-owned
 *     session has `s.id === "orig@peer"` on the wire, but the peer's
 *     projects.json keys by `"orig"`. Slugged sessions key by
 *     `s.slug`, which is never namespaced.
 *
 * Returns an empty array when no session in the request belongs to
 * the folder owner: caller should skip the PATCH entirely so the
 * daemon doesn't see an empty reorder.
 */
export function reorderKeysForFolder(
  reorderedSessions: Session[],
  folderPeer: string | undefined,
): string[] {
  const ownerPeer = folderPeer ?? ''
  return reorderedSessions
    .filter(s => (s.peer ?? '') === ownerPeer)
    .map(s => s.slug || parseSessionHostPath(s.id).originalId)
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
): HostNode[] {
  const project = projects.find(p => p.slug === projectSlug)
  if (!project) return []

  // Mirror sidebar visibility: only include sessions the sidebar would
  // show under this project. Dead non-tracked sessions stay hidden.
  const projectSessions = sessions.filter(s =>
    matchSession(s, projects)?.slug === projectSlug
    && isSessionVisibleInProject(s, project),
  )

  // Bucket by host path.
  const hostBuckets = new Map<string, { path: string[]; sessions: Session[] }>()
  for (const s of projectSessions) {
    const { path } = parseSessionHostPath(s.id)
    const key = path.join('\0') // NUL-separated because peer names can't contain it
    let bucket = hostBuckets.get(key)
    if (!bucket) {
      bucket = { path, sessions: [] }
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
  // Alive first, then newest-first.
  return [...sessions].sort((a, b) => {
    if (a.alive !== b.alive) return a.alive ? -1 : 1
    const ta = new Date(a.created_at || 0).getTime()
    const tb = new Date(b.created_at || 0).getTime()
    return tb - ta
  })
}
