import type { Session } from './types'

/**
 * Lifecycle-action gating for the header session menu, which offers the
 * lifecycle action in a consistent spot across alive and dead states.
 * (For dead sessions the same action is deliberately mirrored as the
 * primary button in ReplayView's action bar.)
 *
 * The verb is the *daemon's* verdict, carried on the wire as
 * `session.relaunch` ('resume' | 'rerun'), because the daemon is the only
 * party that knows whether a recorded conversation can actually be picked
 * up (see services/gmuxd/internal/relaunch). The UI used to guess from the
 * adapter name and from `resumable` alone, which offered "Rerun" on dead
 * shell sessions that the daemon then refused with "session is not
 * resumable" — and, via Restart, killed them on the way.
 *
 * Fallback for pre-`relaunch` daemons (a peer-owned row from an older
 * host): fall back to `resumable` and the adapter-name guess below. It is
 * a guess, and it is confined to this one legacy path.
 */
const RESUMABLE_AGENT_KINDS = new Set(['claude', 'codex', 'pi'])

export type RelaunchVerb = 'resume' | 'rerun'

export type LifecycleAction = {
  id: 'restart' | 'relaunch'
  /** Which relaunch the daemon will perform; undefined for Restart. Used
   * for labels and for naming the action in failure toasts. */
  verb?: RelaunchVerb
  /** Menu-item label ("Resume session") — needs the noun, since the menu
   * also holds non-lifecycle rows. */
  label: string
  /** Button label for ReplayView's action bar ("Resume") — the bar sits
   * inside the session view, so the noun is redundant there. */
  shortLabel: string
  disabled: boolean
}

/** The relaunch verb the owning daemon will honor, or null when it will
 * honor none.
 *
 * `relaunch` is checked first *and* alone: the daemon emits it and
 * `resumable` from the same verdict (wire/convert.go sets both from one
 * `kind`, and clears both together for a dismissed row), so a present verb
 * already implies `resumable`. `resumable` is only consulted when the verb
 * is absent, which means a peer on a daemon older than the field. */
export function relaunchVerb(
  session: Pick<Session, 'relaunch' | 'resumable' | 'adapter'>,
): RelaunchVerb | null {
  if (session.relaunch === 'resume' || session.relaunch === 'rerun') return session.relaunch
  if (!session.resumable) return null
  return RESUMABLE_AGENT_KINDS.has(session.adapter) ? 'resume' : 'rerun'
}

/**
 * The user-visible notice for a relaunch that did not happen where the
 * session used to live. The daemon reports `original_cwd`/`fallback_cwd`
 * only when it substituted a directory (its recorded one is gone); a rerun
 * runs an arbitrary recorded command, so running it somewhere else is a fact
 * the user has to be told rather than a detail to drop.
 *
 * Returns null when nothing was substituted.
 */
export function relaunchDirectoryNotice(
  verb: RelaunchVerb,
  data: Record<string, unknown> | null | undefined,
): string | null {
  const fallback = data?.fallback_cwd
  const original = data?.original_cwd
  if (typeof fallback !== 'string' || !fallback) return null
  const word = verb === 'resume' ? 'Resumed' : 'Rerunning'
  const from = typeof original === 'string' && original ? ` (${original} no longer exists)` : ''
  return `${word} in ${fallback} instead${from}`
}

/**
 * The one lifecycle action a session offers in its current state, or
 * null when it offers none (dead and not relaunchable).
 *
 * - alive → Restart
 * - dead + relaunch 'resume' → Resume
 * - dead + relaunch 'rerun' → Rerun
 * - dead + no relaunch verdict → none
 */
export function lifecycleAction(
  session: Pick<Session, 'alive' | 'relaunch' | 'resumable' | 'adapter'>,
  pending = false,
): LifecycleAction | null {
  if (session.alive) {
    return { id: 'restart', label: 'Restart session', shortLabel: 'Restart', disabled: false }
  }
  const verb = relaunchVerb(session)
  if (!verb) return null
  if (pending) {
    const busy = verb === 'resume' ? 'Resuming…' : 'Rerunning…'
    return { id: 'relaunch', verb, label: busy, shortLabel: busy, disabled: true }
  }
  const word = verb === 'resume' ? 'Resume' : 'Rerun'
  return { id: 'relaunch', verb, label: `${word} session`, shortLabel: word, disabled: false }
}
