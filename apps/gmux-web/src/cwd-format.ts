// Formatting a session's working directory for display on a card,
// relative to its project's canonical folder.
//
// Paths arrive already canonicalized to `~/...` form (projects.ts),
// so no $HOME expansion is needed here — we only decide how to shorten
// a cwd against the project root the card belongs to.

/**
 * Express `cwd` relative to a project's `canonical` folder.
 *
 *   equal        -> ''             (nothing worth showing)
 *   descendant   -> './sub/dir'    (worktree / subfolder)
 *   unrelated    -> cwd            (already ~/-abbreviated absolute)
 *
 * A missing `canonical` (project without a path rule) yields the cwd
 * verbatim: we can't compute a relation, so we show what we have.
 */
export function relativeCwd(cwd: string, canonical?: string): string {
  if (!canonical) return cwd
  if (cwd === canonical) return ''
  if (cwd.startsWith(canonical + '/')) return './' + cwd.slice(canonical.length + 1)
  return cwd
}

/**
 * Card-facing helper: the cwd to surface *only when it differs* from
 * the canonical folder. Returns null when there's nothing to add —
 * an unresolved (empty) cwd, or a cwd that is exactly the project
 * folder. Used by the home dashboard, where the default (session at
 * the project root) should stay quiet.
 */
export function cwdBadge(cwd: string | undefined, canonical?: string): string | null {
  if (!cwd) return null
  const rel = relativeCwd(cwd, canonical)
  return rel === '' ? null : rel
}

/**
 * Project-hub row disambiguator: like relativeCwd, but a session sitting
 * at the project root reads as '.' (a meaningful "here" marker when rows
 * disagree on cwd) rather than a blank segment. An unresolved (empty)
 * cwd still yields '' so the row renders nothing — a placeholder cwd is
 * not a location worth labelling.
 */
export function hubCwdLabel(cwd: string | undefined, canonical?: string): string {
  if (!cwd) return ''
  return relativeCwd(cwd, canonical) || '.'
}
