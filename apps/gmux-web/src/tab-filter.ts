// --- Tab-scope filter selectors (`?filter=` URL param) ---
//
// A tab can be narrowed to a comma-separated list of selectors:
//
//   gmux@*        this project on every host (owned + references)
//   *@server      every project on that host
//   gmux@server   exact project-on-host
//   gmux          shorthand for gmux@*
//
// Multiple selectors are a union. The URL is the persistence layer:
// pin a tab to a project or host, bookmark it, let the browser's tab
// management (windows, groups, PWA icons) do the rest.
//
// Host names are peer names; the viewer's own host matches both its
// hostname and the reserved alias `local`. Pure functions — the store
// wires them to signals.

import type { Session } from './types'

export interface Selector {
  /** Project slug, or '*' for any. */
  project: string
  /** Host (peer name, local hostname, or 'local'), or '*' for any. */
  host: string
}

/** Parse a raw `?filter=` value into selectors. Empty / degenerate
 *  entries (``, `@`, `*@*`, `*`) are dropped: they'd match everything,
 *  which is the same as no selector — but keeping them would render
 *  a lying chip. */
export function parseFilterParam(raw: string | null | undefined): Selector[] {
  if (!raw) return []
  const out: Selector[] = []
  const seen = new Set<string>()
  for (const part of raw.split(',')) {
    const token = part.trim()
    if (!token) continue
    const at = token.indexOf('@')
    const project = (at < 0 ? token : token.slice(0, at)).trim() || '*'
    const host = (at < 0 ? '*' : token.slice(at + 1)).trim() || '*'
    if (project === '*' && host === '*') continue
    // Dedupe: repeated tokens (e.g. hand-authored `gmux,gmux`) would
    // otherwise render duplicate chips that share a key, so removing one
    // removes both.
    const key = `${project}\0${host}`
    if (seen.has(key)) continue
    seen.add(key)
    out.push({ project, host })
  }
  return out
}

/** Serialize selectors back to a `?filter=` value. Inverse of
 *  parseFilterParam for well-formed selectors; `p@*` collapses to the
 *  `p` shorthand people would type by hand. */
export function formatFilterParam(selectors: readonly Selector[]): string {
  return selectors
    .map(s => (s.host === '*' ? s.project : `${s.project}@${s.host}`))
    .join(',')
}

/** Human label for a chip. */
export function selectorLabel(s: Selector): string {
  if (s.host === '*') return s.project
  if (s.project === '*') return `@${s.host}`
  return `${s.project}@${s.host}`
}

/** Does `host` from a selector address the viewer's own host?
 *  `localHostname` is the daemon's hostname (health.hostname). */
function isLocalHostToken(host: string, localHostname: string | undefined): boolean {
  return host === 'local' || (!!localHostname && host === localHostname)
}

/**
 * Whether a project folder is itself in the filter's scope. Used to
 * keep in-scope folders visible even when they have no sessions: a tab
 * pinned to `?filter=gmux` should still show the (empty) gmux folder
 * as a launch target, not a "no sessions match" dead end.
 *
 * A folder's host is its owning peer, or the viewer's host when it is
 * locally owned (`peer` undefined).
 */
export function folderMatchesFilter(
  folder: { slug: string; peer?: string },
  selectors: readonly Selector[],
  localHostname: string | undefined,
): boolean {
  if (selectors.length === 0) return true
  for (const sel of selectors) {
    const projectOk = sel.project === '*' || folder.slug === sel.project
    const hostOk = sel.host === '*' || (
      folder.peer ? folder.peer === sel.host : isLocalHostToken(sel.host, localHostname)
    )
    if (projectOk && hostOk) return true
  }
  return false
}

/**
 * Whether a session survives the filter (union across selectors).
 *
 * Host matching: a session's host is its `peer` name, or the viewer's
 * host when `peer` is unset. Local peers (devcontainers) match by
 * their own peer name — they are distinct hosts from the URL's point
 * of view even though their sessions live in local folders.
 *
 * Project matching uses the stamp (`project_slug`); unstamped sessions
 * only survive `*`-project selectors on a matching host.
 */
export function sessionMatchesFilter(
  s: Pick<Session, 'project_slug' | 'peer'>,
  selectors: readonly Selector[],
  localHostname: string | undefined,
): boolean {
  if (selectors.length === 0) return true
  for (const sel of selectors) {
    const projectOk = sel.project === '*' || s.project_slug === sel.project
    const hostOk = sel.host === '*' || (
      s.peer ? s.peer === sel.host : isLocalHostToken(sel.host, localHostname)
    )
    if (projectOk && hostOk) return true
  }
  return false
}
