import type { Session } from './types'

/** Canonical semantic presentation state for every session status/attention
 * surface. Active is current status and takes precedence over consumable
 * unread attention; durable error only colors whichever of those exists. */
export type SessionPresentationState =
  | 'none'
  | 'active'
  | 'active-error'
  | 'waiting'
  | 'waiting-error'

export function sessionPresentationState(
  session: Pick<Session, 'status' | 'unread'>,
): SessionPresentationState {
  if (session.status?.active) return session.status.error ? 'active-error' : 'active'
  if (session.unread) return session.status?.error ? 'waiting-error' : 'waiting'
  return 'none'
}

export function isWaitingPresentation(state: SessionPresentationState): boolean {
  return state === 'waiting' || state === 'waiting-error'
}
