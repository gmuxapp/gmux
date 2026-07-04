import type { Session } from './types'

/**
 * Lifecycle-action gating for the header session menu, which offers the
 * lifecycle action in a consistent spot across alive and dead states.
 * (For dead sessions the same action is deliberately mirrored as the
 * primary button in ReplayView's action bar.)
 *
 * Adapter kinds whose runners have an explicit resume protocol
 * (--resume <id> or equivalent). Anything not in this set falls back to
 * "Rerun" because there's no captured agent state to pick up from;
 * re-launching just runs the original command again. Listed explicitly
 * so adding a new agent adapter is a deliberate one-line change here,
 * and unknown kinds default to the safe "Rerun" label.
 */
export const RESUMABLE_AGENT_KINDS = new Set(['claude', 'codex', 'pi'])

export type LifecycleAction = {
  id: 'restart' | 'resume'
  label: string
  disabled: boolean
}

/**
 * The one lifecycle action a session offers in its current state, or
 * null when it offers none (dead and not resumable).
 *
 * - alive → Restart
 * - dead + resumable agent (claude/codex/pi) → Resume
 * - dead + resumable non-agent (shell, one-off command) → Rerun
 * - dead + not resumable → none
 */
export function lifecycleAction(
  session: Pick<Session, 'alive' | 'resumable' | 'adapter'>,
  resuming = false,
): LifecycleAction | null {
  if (session.alive) {
    return { id: 'restart', label: 'Restart session', disabled: false }
  }
  if (!session.resumable) return null
  const isAgent = RESUMABLE_AGENT_KINDS.has(session.adapter)
  return {
    id: 'resume',
    label: resuming
      ? (isAgent ? 'Resuming…' : 'Rerunning…')
      : (isAgent ? 'Resume session' : 'Rerun session'),
    disabled: resuming,
  }
}

/**
 * Whether the mobile bottom bar renders for the current selection. It
 * must appear for *any* selected session — alive or dead — because on
 * touch devices its ☰ button is the only way to open the sidebar
 * overlay. Dead sessions get the bar with keys disabled (canSend=false)
 * but the menu reachable.
 */
export function showMobileBar(session: Pick<Session, 'id'> | null): boolean {
  return session !== null
}
