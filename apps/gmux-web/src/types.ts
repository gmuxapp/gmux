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
  /**
   * RFC3339 timestamp of the session's most recent noteworthy state
   * transition (exited / unread on / working on / error on). Set by
   * the owning daemon. Drives the home dashboard's "Recent" section
   * and acts as a stable sort key for activity-ordered views.
   * Unset for brand-new sessions until their first transition.
   */
  last_activity_at?: string
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
  /**
   * True when this folder is a reference whose peer is connected but
   * no longer reports the named slug in its peer_projects. The folder
   * is rendered with a missing indicator so the user can remove it
   * manually. Distinct from a peer simply being disconnected (handled
   * by the PeerLabel offline state).
   */
  missing?: boolean
  /**
   * True when this folder is a reference whose peer name matches no
   * host in the current roster (not on the tailnet online/offline, not
   * manually added, not a devcontainer). The host may have been
   * renamed or removed; the sidebar flags it and the Hosts tab offers
   * a remap/remove. Distinct from `missing` (peer connected, slug
   * gone) and from a disconnected-but-known peer. (refs #270)
   */
  unresolved?: boolean
  sessions: Session[]
}

// --- Project types (server-side state) ---

export interface MatchRule {
  path?: string
  remote?: string
  exact?: boolean // path must match exactly, not as prefix
}

/**
 * One entry in the viewer's projects.json items[]. Two shapes share
 * this type, discriminated by `peer`:
 *
 *   - Owned: `slug` + `match[]` (+ `sessions[]` maintained by server)
 *     A project owned by this host. Match rules drive session
 *     attribution; `sessions[]` is the ordered list of session keys
 *     for sidebar order.
 *   - Reference: `slug` + `peer`
 *     A pointer to a project owned by a peer. The peer's projects.json
 *     is the source of truth for match rules and session order; the
 *     viewer just declares "show this peer's project at this position
 *     in my sidebar."
 */
export interface ProjectItem {
  slug: string
  /** Set when this item is a reference to a peer-owned project. */
  peer?: string
  /**
   * Stable opaque identity (ADR 0007) of the referenced peer, used to
   * resolve the reference even after the peer is renamed. Set only on
   * references; opportunistically backfilled once the peer is
   * reachable. Absent on legacy references and owned projects.
   */
  node_id?: string
  /** Owned-project match rules. Empty/absent for references. */
  match?: MatchRule[]
  /** Server-managed ordering. Absent for references. */
  sessions?: string[]
}

/** True when an item references a peer-owned project rather than
 *  defining one locally. */
export function isReferenceItem(item: ProjectItem): boolean {
  return !!item.peer
}

export interface DiscoveredProject {
  suggested_slug: string
  remote?: string
  paths: string[]
  session_count: number
  active_count: number
  /**
   * Owning host for this suggestion. Absent for local-host discovery;
   * set to the peer name for sessions disclaimed by a connected peer.
   * Drives the host chip in the modal and routes "+ Add" to the
   * correct daemon (proxied if remote).
   */
  peer?: string
  /**
   * Most-recent created_at among the sessions in this group, as an
   * RFC3339 string. Used to sort suggestions by recency.
   */
  last_active?: string
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
  /**
   * True when this peer is conceptually an extension of the host (a
   * devcontainer reachable via PeerConfig.Local = true). Local peers
   * don't own their own project assignments; their sessions are
   * stamped by the parent and bucket into local sidebar folders.
   */
  local?: boolean
  /**
   * How the peer was added — drives the Hosts-tab grouping. One of
   * 'devcontainer' (auto-discovered Docker container) or 'manual'
   * (peers.json / POST /v1/peers, including tailnet hosts). Absent on
   * older daemons. 'tailscale' is legacy (autodiscovery removed in
   * ADR 0008) and is folded into the manual group.
   */
  source?: 'tailscale' | 'devcontainer' | 'manual'
  /**
   * The peer's stable opaque identity (ADR 0007), reported by its
   * /v1/health. References anchor on this so they survive the peer
   * being renamed. Absent for offline peers we've never probed and
   * for pre-ADR-0007 daemons.
   */
  node_id?: string
}

/**
 * One project owned by a connected peer, projected down to just what
 * the viewer needs to render a reference (folder header, launch
 * fallback). Counts and timestamps are derived client-side from
 * stamped sessions.
 */
export interface PeerProject {
  slug: string
  launch_cwd?: string
}

export interface LauncherDef {
  id: string
  label: string
  command: string[]
  description?: string
  available: boolean
}
