// --- URL routing and view resolution ---
//
// Maps between URL paths and the app's view model. Pure functions with
// no side effects or signal dependencies.

import type { Session, ProjectItem } from './types'
import { matchSession } from './projects'

// --- URL parsing ---

/**
 * Parse a URL path into project/host/adapter/slug segments.
 *
 * URL hierarchy:
 *   /<project>/<adapter>/<slug>                       (local project)
 *   /<project>/@<host>/<adapter>/<slug>               (local project, peer session adopted)
 *   /@<owner>/<project>/<adapter>/<slug>              (peer-owned project, ADR 0002)
 *
 * The leading-`@` segment qualifies *project ownership*: the project
 * lives on the named peer's host. The mid-path `@<host>` segment
 * qualifies a *session's host* within a project (used when a viewer
 * has adopted a disclaimed peer session into a local folder).
 */
export function parseSessionPath(path: string): {
  /** Project slug. */
  project?: string
  /** Project owner peer (when path begins with `/@<owner>/...`). */
  projectPeer?: string
  /** Session host within a project (mid-path `@<host>`). */
  host?: string
  adapter?: string
  slug?: string
} {
  // Strip leading slash and split.
  let parts = path.replace(/^\//, '').split('/').filter(Boolean)
  if (parts.length === 0) return {}
  // Skip internal routes.
  if (parts[0] === '_') return {}

  // Leading @<owner> qualifies project ownership.
  let projectPeer: string | undefined
  if (parts[0].startsWith('@')) {
    projectPeer = parts[0].slice(1)
    parts = parts.slice(1)
    if (parts.length === 0) return { projectPeer }
  }

  if (parts.length === 1) return { projectPeer, project: parts[0] }
  // @-prefixed second segment is a session host (peer-adopted session).
  if (parts[1].startsWith('@')) {
    const host = parts[1].slice(1)
    if (parts.length === 2) return { projectPeer, project: parts[0], host }
    if (parts.length === 3) return { projectPeer, project: parts[0], host, adapter: parts[2] }
    return { projectPeer, project: parts[0], host, adapter: parts[2], slug: parts[3] }
  }
  if (parts.length === 2) return { projectPeer, project: parts[0], adapter: parts[1] }
  return { projectPeer, project: parts[0], adapter: parts[1], slug: parts[2] }
}

/**
 * Build a URL path for a session within a project.
 *
 * `projectPeer` qualifies project ownership: when set, the URL is
 * peer-prefixed (`/@<owner>/<slug>/...`), reflecting that the project
 * lives on the named peer (ADR 0002). The session's own host is
 * encoded by the mid-path `@<host>` segment when it differs from the
 * project owner (i.e. a disclaimed peer session adopted into a local
 * folder), and is omitted when project owner and session host match
 * (since the URL form would be redundant).
 */
export function sessionPath(
  projectSlug: string,
  session: { adapter: string; slug?: string; id: string; peer?: string },
  projectPeer?: string,
): string {
  // Fallback for an untitled session (no slug yet): the full gmux session
  // id. It's already short and URL-safe (`sess-<8 hex>`) and unique, unlike
  // a truncation — `id.slice(0, 8)` would keep only `sess-` + 3 hex (~4096
  // values), and resolveSessionFromPath's prefix match returns the first
  // hit, so two untitled sessions could collide onto one URL. Resolution
  // still works: `s.id.startsWith(slug)` matches the full id exactly.
  const slug = session.slug || session.id
  const ownerPrefix = projectPeer ? `/@${projectPeer}` : ''
  // Mid-path session-host segment is needed only when the session
  // lives on a different host than the project owner.
  const sessionHost = session.peer && session.peer !== projectPeer ? `/@${session.peer}` : ''
  return `${ownerPrefix}/${projectSlug}${sessionHost}/${session.adapter}/${slug}`
}

/** Query params that define a tab's identity (its narrowing scope and
 *  sidebar view). In-app links must carry them so navigation within a
 *  pinned tab doesn't silently un-pin it. */
const TAB_PARAMS = ['filter', 'sidebar'] as const

/** Append the tab-identity params from `search` (a location.search
 *  string) onto `path`. Other params (e.g. ?settings) are not carried:
 *  they're transient UI state, not tab identity. */
export function withTabParams(path: string, search: string): string {
  const current = new URLSearchParams(search)
  const carried = new URLSearchParams()
  for (const key of TAB_PARAMS) {
    const v = current.get(key)
    if (v) carried.set(key, v)
  }
  const qs = carried.toString()
  return qs ? `${path}?${qs}` : path
}

/**
 * Resolve a parsed URL path to a session ID.
 * Returns null if no matching session is found.
 */
export function resolveSessionFromPath(
  parsed: { project?: string; projectPeer?: string; host?: string; adapter?: string; slug?: string },
  projects: ProjectItem[],
  sessions: Session[],
): string | null {
  if (!parsed.project) return null

  // Sessions belonging to the addressed project (ADR 0002):
  //  - Peer-owned project: stamp matches `(peer, project_slug)`.
  //  - Local-owned project: stamp matches `("", project_slug)` *or*
  //    the viewer's match rules adopt a disclaimed session.
  // The mid-path `@<host>` segment further filters session host
  // (used when a local folder contains adopted peer sessions).
  const filterHost = parsed.host
  const projectSessions = sessions.filter(s => {
    if (parsed.projectPeer) {
      // Peer-owned project: trust the stamp.
      if (s.peer !== parsed.projectPeer) return false
      if (s.project_slug !== parsed.project) return false
    } else {
      // Local project: claimed-local OR disclaimed-adopted.
      const claimedHere = !s.peer && s.project_slug === parsed.project
      const adoptedHere = !s.project_slug
        && matchSession(s, projects)?.slug === parsed.project
      if (!claimedHere && !adoptedHere) return false
    }
    if (filterHost !== undefined && s.peer !== filterHost) return false
    if (filterHost === undefined && !parsed.projectPeer && s.peer) return false
    return true
  })

  // For local projects we still require the project to exist in the
  // viewer's projects list. Peer-owned projects are valid as long as
  // they have at least one matching session in view.
  if (!parsed.projectPeer && !projects.find(p => p.slug === parsed.project)) {
    return null
  }
  if (parsed.projectPeer && projectSessions.length === 0) {
    return null
  }

  // Drop the unused `project` const that the original code held; we
  // already validated existence above and only need the session list.

  if (!parsed.adapter) {
    // /:project only - return first alive session, or first session.
    const alive = projectSessions.find(s => s.alive)
    return alive?.id ?? projectSessions[0]?.id ?? null
  }

  // Filter by adapter kind.
  const adapterSessions = projectSessions.filter(s => s.adapter === parsed.adapter)

  if (!parsed.slug) {
    // /:project/:adapter only - return first alive, or first.
    const alive = adapterSessions.find(s => s.alive)
    return alive?.id ?? adapterSessions[0]?.id ?? null
  }

  // Full match: /:project/:adapter/:slug
  // Try exact slug match first, then prefix match on session ID.
  const exact = adapterSessions.find(s => s.slug === parsed.slug)
  if (exact) return exact.id

  const byId = adapterSessions.find(s => s.id.startsWith(parsed.slug!))
  return byId?.id ?? null
}

// --- View state model ---

/**
 * The top-level thing the app is currently showing. Derived from the URL
 * and drives what the main panel renders.
 *
 *  - `home`: overview / landing (activity dashboard, quick-launch)
 *  - `session`: a specific terminal session, by id.
 *
 * There is no project-hub view: project navigation lives entirely in the
 * sidebar (folders collapse/expand in place), and a bare `/:project` URL
 * redirects to home.
 */
export type View =
  | { kind: 'home' }
  | { kind: 'session'; sessionId: string }

/** Structural equality for views. */
export function viewsEqual(a: View, b: View): boolean {
  if (a.kind !== b.kind) return false
  switch (a.kind) {
    case 'home': return true
    case 'session': return a.sessionId === (b as { sessionId: string }).sessionId
  }
}

/**
 * Resolve a URL path to a View given the current projects and sessions.
 *
 * Rules:
 *  - `/` (or unparseable / internal) -> home
 *  - `/:project` (no adapter) -> home (project hubs are retired; folder
 *    navigation lives in the sidebar)
 *  - `/:project/:adapter[/:slug]` where a session resolves -> session view
 *  - `/:project/:adapter[/:slug]` with no matching session -> home
 */
export function resolveViewFromPath(
  path: string,
  projects: ProjectItem[],
  sessions: Session[],
): View {
  const parsed = parseSessionPath(path)
  if (!parsed.project) return { kind: 'home' }

  // A bare project URL no longer has a destination of its own — send it
  // home. Only session URLs (with an adapter segment) resolve to a view.
  if (!parsed.adapter) return { kind: 'home' }

  // /:project/:adapter[/:slug] resolves to a concrete session when
  // possible; anything else (project gone, session gone) falls to home.
  const sessionId = resolveSessionFromPath(parsed, projects, sessions)
  if (sessionId) return { kind: 'session', sessionId }
  return { kind: 'home' }
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
    case 'session': {
      const sess = sessions.find(s => s.id === view.sessionId)
      if (!sess) return null
      // Peer-owned project (ADR 0002): URL is peer-prefixed; session
      // lives on the project's owner.
      if (sess.project_slug && sess.peer) {
        return sessionPath(sess.project_slug, sess, sess.peer)
      }
      // Local-claimed: project owner is the viewer.
      if (sess.project_slug && !sess.peer) {
        return sessionPath(sess.project_slug, sess)
      }
      // Disclaimed: viewer's match rules decide the local folder.
      const project = matchSession(sess, projects)
      if (!project) return null
      return sessionPath(project.slug, sess)
    }
  }
}
