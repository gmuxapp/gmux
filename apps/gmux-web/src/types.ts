// --- Data types ---
//
// Pure interfaces for the frontend data model. No logic, no imports.

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
  slug?: string
  /** Version string of the gmux runner binary that owns this session. */
  runner_version?: string
  /** SHA-256 of the gmux runner binary (first 8 chars useful for display). */
  binary_hash?: string
}

export interface Folder {
  name: string       // display name (project slug or derived name)
  path: string       // project slug (used as key)
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

/** Mirrors /v1/health peers entries. */
export interface PeerInfo {
  name: string
  url: string
  status: string // 'connected' | 'connecting' | 'disconnected' | 'offline'
  session_count: number
  last_error?: string
  version?: string
  default_launcher?: string
  launchers?: LauncherDef[]
}

export interface LauncherDef {
  id: string
  label: string
  command: string[]
  description?: string
  available: boolean
}
