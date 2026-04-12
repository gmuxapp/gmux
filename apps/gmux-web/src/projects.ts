// --- Project-session matching and topology ---
//
// Maps sessions to projects using match rules (path prefix, git remote).
// Builds sidebar folders and project hub topology. Pure functions with
// no side effects or signal dependencies.

import type { Session, Folder, ProjectItem, PeerInfo } from './types'

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

// --- Sidebar folders ---

/**
 * Build Folder[] from configured projects + live sessions.
 * Each project becomes a folder with its matched sessions.
 * Order follows the project list order (user-controlled).
 */
export function buildProjectFolders(
  projects: ProjectItem[],
  sessions: Session[],
): Folder[] {
  const buckets = new Map<string, Session[]>()
  for (const project of projects) {
    buckets.set(project.slug, [])
  }

  // Build a lookup of session keys per project for dead-session filtering.
  // Alive sessions show by match rules (immediate, no auto-assign lag).
  // Dead sessions only show if they're in the project's sessions array
  // (i.e., they were alive while the project existed).
  const arrayKeys = new Map<string, Set<string>>()
  for (const project of projects) {
    arrayKeys.set(project.slug, new Set(project.sessions ?? []))
  }

  for (const session of sessions) {
    const matched = matchSession(session, projects)
    if (!matched || !buckets.has(matched.slug)) continue

    if (session.alive) {
      buckets.get(matched.slug)!.push(session)
    } else {
      // Dead session: only include if it's in the project's sessions array.
      const keys = arrayKeys.get(matched.slug)!
      const key = session.slug || session.id
      if (keys.has(key) || keys.has(session.id)) {
        buckets.get(matched.slug)!.push(session)
      }
    }
  }

  const folders: Folder[] = []
  for (const project of projects) {
    const matched = buckets.get(project.slug) || []
    folders.push({
      name: project.slug,
      path: project.slug,
      launchCwd: project.match.find(r => r.path)?.path,
      sessions: matched.sort((a, b) => {
        // The sessions array is the canonical order. Sessions not yet
        // in the array (just spawned, auto-assign pending) go at the
        // end sorted by creation time.
        const keys = project.sessions ?? []
        const keyOf = (s: Session) => s.slug || s.id
        const ai = keys.indexOf(keyOf(a))
        const bi = keys.indexOf(keyOf(b))
        if (ai !== -1 && bi !== -1) return ai - bi
        if (ai !== -1) return -1
        if (bi !== -1) return 1
        return new Date(a.created_at).getTime() - new Date(b.created_at).getTime()
      }),
    })
  }

  return folders
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
