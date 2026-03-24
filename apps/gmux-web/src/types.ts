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
  resume_key?: string
  socket_path: string
  terminal_cols?: number
  terminal_rows?: number
  shell_title?: string
  adapter_title?: string
  binary_hash?: string
  stale?: boolean
}

export interface Folder {
  name: string      // display name (basename of workspace root or cwd)
  path: string      // workspace root path, or cwd if no workspace
  sessions: Session[]
}

/**
 * Group sessions into folders. Sessions sharing a workspace_root are
 * collapsed into a single folder named after the root. Sessions without
 * a workspace root (or whose root is unique) become standalone folders.
 */
export function groupByFolder(sessions: Session[]): Folder[] {
  // Step 1: bucket sessions by their grouping key.
  // Key is workspace_root when set, otherwise cwd.
  const buckets = new Map<string, Session[]>()
  for (const s of sessions) {
    const key = s.workspace_root || s.cwd
    const existing = buckets.get(key) || []
    existing.push(s)
    buckets.set(key, existing)
  }

  // Step 2: build folders from buckets.
  const folders: Folder[] = []
  for (const [key, bucketSessions] of buckets) {
    const parts = key.split('/')
    folders.push({
      name: parts[parts.length - 1],
      path: key,
      sessions: bucketSessions.sort((a, b) => {
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
