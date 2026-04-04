export interface SessionStatus {
  label: string
  working: boolean
  error?: boolean
}

export interface Session {
  id: string
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
  stale?: boolean
}

export interface Folder {
  name: string      // display name (basename of workspace root or cwd)
  path: string      // workspace root path, or cwd if no workspace
  sessions: Session[]
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
