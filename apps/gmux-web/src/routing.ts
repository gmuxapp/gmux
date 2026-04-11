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
  session: { kind: string; slug?: string; id: string; peer?: string },
): string {
  const slug = session.slug || session.id.slice(0, 8)
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

  const project = projects.find(p => p.slug === parsed.project)
  if (!project) return { kind: 'home' }

  // /:project alone goes straight to the project hub.
  if (!parsed.adapter) return { kind: 'project', projectSlug: project.slug }

  // /:project/:adapter[/:slug] resolves to a concrete session when possible.
  const sessionId = resolveSessionFromPath(parsed, projects, sessions)
  if (sessionId) return { kind: 'session', sessionId }

  // URL pointed at a session that no longer exists -> fall back to hub.
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
