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

  /**
   * Project assignment stamps from the session's origin host (ADR 0002).
   * Set as a pair, only by the origin's `Reconcile`:
   *  - non-empty `project_slug`: the session is *claimed* by the named
   *    project on its origin; folder = `(peer, project_slug)`,
   *    sort key = `project_index`.
   *  - empty / absent: the session is *disclaimed*; viewers fall back
   *    to their own match rules (free-game adoption).
   * Index defaults to 0 on decode, which is also a valid first-position
   * stamp; meaningful only when `project_slug` is non-empty.
   */
  project_slug?: string
  project_index?: number
}

export interface Folder {
  /**
   * Stable identity for React keys and selection. Local folders use the
   * project slug; peer folders use `${peer}::${slug}` to disambiguate
   * same-named projects on different hosts (ADR 0002).
   */
  key: string
  /** Display name (project slug, possibly colliding across hosts). */
  name: string
  /** Project slug (used for URL routing and local projects.json keying). */
  slug: string
  /**
   * Origin host name when this folder is owned by a peer; absent when
   * owned by the viewer. Drives peer-label rendering and the launch
   * button's `peer=` argument.
   */
  peer?: string
  /** Filesystem path hint for launching new sessions in this folder. */
  launchCwd?: string
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
  status: string // 'connected' | 'connecting' | 'disconnected' | 'offline' (connecting treated as disconnected in UI)
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
