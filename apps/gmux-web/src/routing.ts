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
  session: { kind: string; slug?: string; id: string; peer?: string },
  projectPeer?: string,
): string {
  const slug = session.slug || session.id.slice(0, 8)
  const ownerPrefix = projectPeer ? `/@${projectPeer}` : ''
  // Mid-path session-host segment is needed only when the session
  // lives on a different host than the project owner.
  const sessionHost = session.peer && session.peer !== projectPeer ? `/@${session.peer}` : ''
  return `${ownerPrefix}/${projectSlug}${sessionHost}/${session.kind}/${slug}`
}

/** Build the URL for a project hub (no session). */
export function projectPath(slug: string, peer?: string): string {
  return peer ? `/@${peer}/${slug}` : `/${slug}`
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
  const adapterSessions = projectSessions.filter(s => s.kind === parsed.adapter)

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
 *  - `home`: overview / landing (host status, projects, quick-launch)
 *  - `project`: the project hub page for a single project. `projectPeer`
 *    is set for peer-owned projects (ADR 0002); absent for local.
 *  - `session`: a specific terminal session, by id.
 */
export type View =
  | { kind: 'home' }
  | { kind: 'project'; projectSlug: string; projectPeer?: string }
  | { kind: 'session'; sessionId: string }

/** Structural equality for views. */
export function viewsEqual(a: View, b: View): boolean {
  if (a.kind !== b.kind) return false
  switch (a.kind) {
    case 'home': return true
    case 'project': {
      const bp = b as { projectSlug: string; projectPeer?: string }
      return a.projectSlug === bp.projectSlug && a.projectPeer === bp.projectPeer
    }
    case 'session': return a.sessionId === (b as { sessionId: string }).sessionId
  }
}

/**
 * Resolve a URL path to a View given the current projects and sessions.
 *
 * Rules:
 *  - `/` (or unparseable / internal) -> home
 *  - `/:project` where project is unknown -> home
 *  - `/:project` where project exists -> project (hub page)
 *  - `/:project/:adapter[/:slug]` where a session resolves -> session view
 *  - `/:project/:adapter[/:slug]` with no matching session but project
 *    exists -> project view (hub)
 */
export function resolveViewFromPath(
  path: string,
  projects: ProjectItem[],
  sessions: Session[],
): View {
  const parsed = parseSessionPath(path)
  if (!parsed.project) return { kind: 'home' }

  // Validate that the addressed project exists in the viewer's view.
  if (parsed.projectPeer) {
    // Peer-owned: project exists iff at least one session carries the
    // matching `(peer, slug)` stamp. We don't sync peer projects
    // separately (ADR 0002); empty peer projects don't render.
    const hasOne = sessions.some(
      s => s.peer === parsed.projectPeer && s.project_slug === parsed.project,
    )
    if (!hasOne) return { kind: 'home' }
  } else {
    if (!projects.find(p => p.slug === parsed.project)) return { kind: 'home' }
  }

  const projectView: View = {
    kind: 'project',
    projectSlug: parsed.project,
    ...(parsed.projectPeer ? { projectPeer: parsed.projectPeer } : {}),
  }

  // /:project alone goes straight to the project hub.
  if (!parsed.adapter) return projectView

  // /:project/:adapter[/:slug] resolves to a concrete session when possible.
  const sessionId = resolveSessionFromPath(parsed, projects, sessions)
  if (sessionId) return { kind: 'session', sessionId }

  // URL pointed at a session that no longer exists -> fall back to hub.
  return projectView
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
      return projectPath(view.projectSlug, view.projectPeer)
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
