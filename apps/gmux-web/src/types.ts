// --- URL routing ---

/**
 * Parse a URL path into project/host/adapter/slug segments.
 *
 * URL hierarchy:
 *   /<project>/<adapter>/<slug>              (local)
 *   /<project>/@<host>/<adapter>/<slug>       (remote, future)
 *
 * The @-prefix on the second segment distinguishes a remote host
 * from an adapter name.
 */
export function parseSessionPath(path: string): {
  project?: string
  host?: string
  adapter?: string
  slug?: string
} {
  // Strip leading slash and split.
  const parts = path.replace(/^\//, '').split('/').filter(Boolean)
  if (parts.length === 0) return {}
  // Skip internal routes.
  if (parts[0] === '_') return {}
  if (parts.length === 1) return { project: parts[0] }
  // @-prefixed second segment is a remote host (future: aggregation).
  if (parts[1].startsWith('@')) {
    const host = parts[1].slice(1)
    if (parts.length === 2) return { project: parts[0], host }
    if (parts.length === 3) return { project: parts[0], host, adapter: parts[2] }
    return { project: parts[0], host, adapter: parts[2], slug: parts[3] }
  }
  if (parts.length === 2) return { project: parts[0], adapter: parts[1] }
  return { project: parts[0], adapter: parts[1], slug: parts[2] }
}

/** Build a URL path for a session within a project. */
export function sessionPath(
  projectSlug: string,
  session: { kind: string; resume_key?: string; id: string; peer?: string },
): string {
  const slug = session.resume_key || session.id.slice(0, 8)
  if (session.peer) {
    return `/${projectSlug}/@${session.peer}/${session.kind}/${slug}`
  }
  return `/${projectSlug}/${session.kind}/${slug}`
}

/**
 * Resolve a parsed URL path to a session ID.
 * Returns null if no matching session is found.
 */
export function resolveSessionFromPath(
  parsed: { project?: string; host?: string; adapter?: string; slug?: string },
  projects: ProjectItem[],
  sessions: Session[],
): string | null {
  if (!parsed.project) return null
  // Filter by remote host when the URL has an @host segment.
  const filterPeer = parsed.host ?? undefined

  // Find the project by slug.
  const project = projects.find(p => p.slug === parsed.project)
  if (!project) return null

  // Match sessions to this project, filtering by peer when URL has @host.
  const projectSessions = sessions.filter(
    s => matchSession(s, projects)?.slug === parsed.project
      && (filterPeer === undefined ? !s.peer : s.peer === filterPeer),
  )

  if (!parsed.adapter) {
    // /:project only - return first alive session, or first session.
    const alive = projectSessions.find(s => s.alive)
    return alive?.id ?? projectSessions[0]?.id ?? null
  }

  // Filter by adapter kind.
  const adapterSessions = projectSessions.filter(s => s.kind === parsed.adapter)

  if (!parsed.slug) {
    // /:project/:adapter only - return first alive, or first.
    const alive = adapterSessions.find(s => s.alive)
    return alive?.id ?? adapterSessions[0]?.id ?? null
  }

  // Full match: /:project/:adapter/:slug
  // Try exact slug match first, then prefix match on session ID.
  const exact = adapterSessions.find(s => s.resume_key === parsed.slug)
  if (exact) return exact.id

  const byId = adapterSessions.find(s => s.id.startsWith(parsed.slug!))
  return byId?.id ?? null
}

// --- View state model ---

/**
 * The top-level thing the app is currently showing. Derived from the URL
 * and drives what the main panel renders.
 *
 *  - `home`: overview / landing (host status, projects, quick-launch)
 *    future cross-project home page).
 *  - `project`: the project hub page for a single project.
 *  - `session`: a specific terminal session, by id.
 */
export type View =
  | { kind: 'home' }
  | { kind: 'project'; projectSlug: string }
  | { kind: 'session'; sessionId: string }

/** Structural equality for views. */
export function viewsEqual(a: View, b: View): boolean {
  if (a.kind !== b.kind) return false
  switch (a.kind) {
    case 'home': return true
    case 'project': return a.projectSlug === (b as { projectSlug: string }).projectSlug
    case 'session': return a.sessionId === (b as { sessionId: string }).sessionId
  }
}

/**
 * Resolve a URL path to a View given the current projects and sessions.
 *
 * Rules:
 *  - `/` (or unparseable / internal) → home
 *  - `/:project` where project is unknown → home
 *  - `/:project` where project exists → project (hub page)
 *  - `/:project/:adapter[/:slug]` where a session resolves → session view
 *  - `/:project/:adapter[/:slug]` with no matching session but project
 *    exists → project view (hub)
 */
export function resolveViewFromPath(
  path: string,
  projects: ProjectItem[],
  sessions: Session[],
): View {
  const parsed = parseSessionPath(path)
  if (!parsed.project) return { kind: 'home' }

  const project = projects.find(p => p.slug === parsed.project)
  if (!project) return { kind: 'home' }

  // /:project alone goes straight to the project hub.
  if (!parsed.adapter) return { kind: 'project', projectSlug: project.slug }

  // /:project/:adapter[/:slug] resolves to a concrete session when possible.
  const sessionId = resolveSessionFromPath(parsed, projects, sessions)
  if (sessionId) return { kind: 'session', sessionId }

  // URL pointed at a session that no longer exists → fall back to hub.
  return { kind: 'project', projectSlug: project.slug }
}

/**
 * Serialize a View to a URL path. Returns null when the view can't be
 * serialized (e.g., session view pointing at a session we no longer have
 * in the store). Callers should leave the URL alone in that case.
 */
export function viewToPath(
  view: View,
  projects: ProjectItem[],
  sessions: Session[],
): string | null {
  switch (view.kind) {
    case 'home':
      return '/'
    case 'project':
      return `/${view.projectSlug}`
    case 'session': {
      const sess = sessions.find(s => s.id === view.sessionId)
      if (!sess) return null
      const project = matchSession(sess, projects)
      if (!project) return null
      return sessionPath(project.slug, sess)
    }
  }
}

export interface SessionStatus {
  label: string
  working: boolean
  error?: boolean
}

export interface Session {
  id: string
  /** Display name of the peer this session runs on. Absent = local. */
  peer?: string
  created_at: string
  command: string[]
  cwd: string
  workspace_root?: string
  remotes?: Record<string, string>
  kind: string
  alive: boolean
  pid: number | null
  exit_code: number | null
  started_at: string
  exited_at: string | null
  title: string
  subtitle: string
  status: SessionStatus | null
  unread: boolean
  resumable?: boolean
  socket_path: string
  terminal_cols?: number
  terminal_rows?: number
  resume_key?: string
  stale?: boolean
}

export interface Folder {
  name: string      // display name (project slug or derived name)
  path: string      // project slug (used as key)
  launchCwd?: string // filesystem path for launching new sessions
  sessions: Session[]
}

// --- Project types (server-side state) ---

export interface MatchRule {
  path?: string
  remote?: string
  hosts?: string[]
  exact?: boolean // path must match exactly, not as prefix
}

export interface ProjectItem {
  slug: string
  match: MatchRule[]
  sessions?: string[] // managed by server; must be preserved in PUT
}

export interface DiscoveredProject {
  suggested_slug: string
  remote?: string
  paths: string[]
  session_count: number
  active_count: number
}

/** Mirrors /v1/peers entries. */
export interface PeerInfo {
  name: string
  url: string
  status: string // 'connected' | 'connecting' | 'disconnected' | 'offline'
  session_count: number
  last_error?: string
}

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

// --- Project-session matching (mirrors Go State.Match) ---

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
  const key = session.resume_key || session.id
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
      const key = session.resume_key || session.id
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
        if (a.cwd !== b.cwd) return a.cwd < b.cwd ? -1 : 1
        return new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
      }),
    })
  }

  return folders
}

/**
 * Group sessions into folders. Grouping priority:
 *
 * 1. Shared remotes: sessions whose repos share any normalized remote URL
 *    are grouped together. This handles forks (origin vs upstream) and
 *    cross-machine grouping (same repo cloned on different machines).
 * 2. Workspace root: sessions sharing a workspace_root (jj workspaces,
 *    git worktrees) are grouped together.
 * 3. cwd: fallback for repos with no remotes and no workspace root.
 */
export function groupByFolder(sessions: Session[]): Folder[] {
  // Step 1: Build groups using union-find on shared remote URLs.
  // Two sessions that share any remote value end up in the same group.
  const parent = new Map<string, string>()  // session ID -> group representative
  const remoteToGroup = new Map<string, string>()  // remote URL -> group representative

  function find(id: string): string {
    let root = id
    while (parent.get(root) !== root) root = parent.get(root)!
    // Path compression
    let cur = id
    while (cur !== root) {
      const next = parent.get(cur)!
      parent.set(cur, root)
      cur = next
    }
    return root
  }

  function union(a: string, b: string) {
    const ra = find(a), rb = find(b)
    if (ra !== rb) parent.set(ra, rb)
  }

  // Initialize each session as its own group.
  for (const s of sessions) parent.set(s.id, s.id)

  // Merge sessions that share any remote URL.
  for (const s of sessions) {
    if (!s.remotes) continue
    for (const url of Object.values(s.remotes)) {
      const existing = remoteToGroup.get(url)
      if (existing) {
        union(s.id, existing)
      } else {
        remoteToGroup.set(url, s.id)
      }
    }
  }

  // Merge sessions that share a workspace_root (but weren't already merged by remotes).
  const wsToGroup = new Map<string, string>()
  for (const s of sessions) {
    const ws = s.workspace_root
    if (!ws) continue
    const existing = wsToGroup.get(ws)
    if (existing) {
      union(s.id, existing)
    } else {
      wsToGroup.set(ws, s.id)
    }
  }

  // Merge sessions that share a cwd (but have no remotes or workspace_root).
  const cwdToGroup = new Map<string, string>()
  for (const s of sessions) {
    if (s.remotes && Object.keys(s.remotes).length > 0) continue
    if (s.workspace_root) continue
    const existing = cwdToGroup.get(s.cwd)
    if (existing) {
      union(s.id, existing)
    } else {
      cwdToGroup.set(s.cwd, s.id)
    }
  }

  // Collect sessions into their final groups.
  const groups = new Map<string, Session[]>()
  for (const s of sessions) {
    const root = find(s.id)
    const list = groups.get(root) || []
    list.push(s)
    groups.set(root, list)
  }

  // Step 2: build folders from groups.
  const folders: Folder[] = []
  for (const groupSessions of groups.values()) {
    const { name, path } = folderIdentity(groupSessions)
    folders.push({
      name,
      path,
      launchCwd: groupSessions[0]?.workspace_root || groupSessions[0]?.cwd || path,
      sessions: groupSessions.sort((a, b) => {
        if (a.cwd !== b.cwd) return a.cwd < b.cwd ? -1 : 1
        return new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
      }),
    })
  }

  // Step 3: sort folders. Working first, then alive, then rest.
  const statePriority = (f: Folder): number => {
    if (f.sessions.some(s => s.alive && s.status?.working)) return 0
    if (f.sessions.some(s => s.alive)) return 1
    return 2
  }
  return folders.sort((a, b) => {
    const pa = statePriority(a)
    const pb = statePriority(b)
    if (pa !== pb) return pa - pb
    const aMax = Math.max(...a.sessions.map(s => new Date(s.created_at).getTime()))
    const bMax = Math.max(...b.sessions.map(s => new Date(s.created_at).getTime()))
    return bMax - aMax
  })
}

/**
 * Derive the display name and grouping path for a folder.
 *
 * Priority:
 * 1. Most common remote URL (e.g. "github.com/gmuxapp/gmux" -> "gmux")
 * 2. workspace_root basename
 * 3. cwd basename
 */
function folderIdentity(sessions: Session[]): { name: string; path: string } {
  // Find the most common remote URL across all sessions in the group.
  const urlCounts = new Map<string, number>()
  for (const s of sessions) {
    if (!s.remotes) continue
    for (const url of Object.values(s.remotes)) {
      urlCounts.set(url, (urlCounts.get(url) || 0) + 1)
    }
  }

  if (urlCounts.size > 0) {
    let bestURL = ''
    let bestCount = 0
    for (const [url, count] of urlCounts) {
      if (count > bestCount) {
        bestURL = url
        bestCount = count
      }
    }
    // Use the repo name from the URL (last path segment).
    const parts = bestURL.split('/')
    return { name: parts[parts.length - 1], path: bestURL }
  }

  // Fallback: workspace_root or cwd from the first session.
  const key = sessions[0].workspace_root || sessions[0].cwd
  const parts = key.split('/')
  return { name: parts[parts.length - 1], path: key }
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
 *   "sess-abc"            → { originalId: "sess-abc", path: [] }
 *   "sess-abc@spoke"      → { originalId: "sess-abc", path: ["spoke"] }
 *   "sess-abc@dev@spoke"  → { originalId: "sess-abc", path: ["spoke", "dev"] }
 *
 * The `@` chain is innermost-first (peering.NamespaceID *appends* when
 * propagating up), so reversing gives the outermost-first path a human
 * reads root → leaf.
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

  // Convert each bucket → HostNode with cwd-grouped folders.
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
