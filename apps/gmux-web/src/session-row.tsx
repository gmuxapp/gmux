// Wide session row used on the home dashboard and (later) the
// rewritten project page. Companion to the sidebar's compact
// SessionItem: same data, more room to breathe. Two lines, optional
// metadata (project / host / cwd / age), shared dot-state and
// unavailability logic via store helpers.
//
// Kept deliberately separate from SessionItem rather than unified
// behind a density prop: the sidebar variant is dense, drag-aware,
// and folder-scoped; this variant is loose and standalone. A single
// component would accumulate flags faster than it would save code.

import { Fragment } from 'preact'
import type { Session } from './types'
import {
  activityMap, peerStatusByName,
  sessionDotState, isSessionUnavailable,
  duplicateConversationFiles,
} from './store'
import { useArrivalPulse } from './use-arrival-pulse'
import { HostSuffix } from './host-suffix'

export interface SessionRowProps {
  session: Session
  /** Destination URL for the row click (deep link to session view). */
  href: string
  /** Currently selected: suppresses unread/error dot. */
  selected?: boolean
  /** Currently resuming: forces the dot into a working state. */
  resuming?: boolean
  /** Show the project name on line 2 (off on the project page). */
  showProject?: boolean
  /** Project display name to render when showProject is true. */
  projectName?: string
  /** Show the owning peer's host suffix on line 2. */
  showHost?: boolean
  /** Show the session's cwd on line 2. */
  showCwd?: boolean
  /** Pre-formatted cwd string. Pass-through; caller decides whether
   *  to render full path or project-relative. */
  cwdLabel?: string
  /** Click handler for navigation side-effects (e.g. close mobile
   *  sidebar). Default link navigation always happens via href. */
  onClick?: () => void
  /** Dismiss/kill button. Hidden when undefined. */
  onClose?: () => void
}

/** Compact "Nm" / "Nh" / "Nd" relative-time formatter for ages on the
 *  dashboard. Sub-minute ages collapse to "now" so the recently-
 *  transitioned row doesn't visibly change every second. */
function formatAge(stampIso: string | undefined, now: number): string | null {
  if (!stampIso) return null
  const t = Date.parse(stampIso)
  if (!Number.isFinite(t)) return null
  const secs = Math.max(0, Math.floor((now - t) / 1000))
  if (secs < 60) return 'now'
  const mins = Math.floor(secs / 60)
  if (mins < 60) return `${mins}m`
  const hours = Math.floor(mins / 60)
  if (hours < 24) return `${hours}h`
  const days = Math.floor(hours / 24)
  return `${days}d`
}

export function SessionRow({
  session,
  href,
  selected,
  resuming,
  showProject,
  projectName,
  showHost,
  showCwd,
  cwdLabel,
  onClick,
  onClose,
}: SessionRowProps) {
  const am = activityMap.value
  const peerStatus = peerStatusByName.value
  const unavailable = isSessionUnavailable(session, peerStatus)
  const sleeping = !session.alive && session.resumable

  const rawDot = resuming ? 'working' : sessionDotState(session, am)
  // Selection mutes attention-grabbing dots (mirrors sidebar
  // behavior): if you're already looking at it, "unread" / "error"
  // are no longer useful signals.
  const dot = (selected && (rawDot === 'error' || rawDot === 'unread')) ? 'none' : rawDot
  const arrival = useArrivalPulse(dot)

  // Age sourced from the same field that drives Recent partitioning:
  // last_activity_at is the canonical "when did anything notable
  // happen here" timestamp. Falls back to created_at for sessions
  // that haven't transitioned yet (matches the dashboard sort).
  const age = formatAge(session.last_activity_at ?? session.created_at, Date.now())

  // Exit code: dead sessions surface "exited (N)" so the row never goes
  // silent about why a session is dead. Live status (working/error) is
  // conveyed by the dot, not text.
  const statusText = !session.alive && session.exit_code != null
    ? `exited (${session.exit_code})`
    : null

  const cls = [
    'session-row',
    selected ? 'selected' : '',
    unavailable ? 'unavailable' : '',
  ].filter(Boolean).join(' ')

  // Line-2 metadata segments. Render only the ones requested by
  // props; empty arrays produce no separator dots in the output.
  const metaSegments: preact.ComponentChildren[] = []
  if (showProject && projectName) {
    metaSegments.push(<span class="session-row-project">{projectName}</span>)
  }
  if (showCwd && cwdLabel) {
    metaSegments.push(<span class="session-row-cwd">{cwdLabel}</span>)
  }
  if (statusText) {
    metaSegments.push(<span class="session-row-status">{statusText}</span>)
  }
  // Same conversation file open in another live runner (ADR 0011 N:1).
  if (session.conversation_file && duplicateConversationFiles.value.has(session.conversation_file)) {
    metaSegments.push(
      <span class="session-row-dup" title="This conversation is open in more than one tab">⚠ open elsewhere</span>,
    )
  }
  if (age) {
    metaSegments.push(<span class="session-row-age">{age}</span>)
  }

  return (
    <a
      class={cls}
      href={href}
      onClick={() => onClick?.()}
      onAuxClick={(e) => {
        if (e.button === 1 && onClose) { e.preventDefault(); onClose() }
      }}
    >
      {unavailable
        ? <svg class="session-unavailable-icon" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"><title>Peer unavailable</title><path d="M2 2 L10 10 M10 2 L2 10" /></svg>
        : sleeping
        ? <svg class="session-sleep-icon" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><title>Resumable</title><path d="M7 1h4l-4 4h4" /><path d="M1 5h5l-5 6h5" /></svg>
        : <span class={`session-dot-indicator ${dot}${arrival ? ` ${arrival}` : ''}`} />
      }
      <div class="session-row-content">
        <div class="session-row-title">{session.title}</div>
        {(metaSegments.length > 0 || showHost) && (
          <div class="session-row-meta">
            {metaSegments.map((seg, i) => (
              // Long-form Fragment carries a key: the shorthand `<>`
              // can't, and without one Preact falls back to index
              // diffing across renders. The visible content here is
              // entirely positional (segment N is whatever segment N
              // happens to be this render), so index is the honest
              // key.
              <Fragment key={i}>
                {i > 0 && <span class="session-row-sep"> · </span>}
                {seg}
              </Fragment>
            ))}
            {showHost && <HostSuffix peer={session.peer} leading={metaSegments.length > 0} />}
          </div>
        )}
      </div>
      {onClose && (
        <button
          class="session-row-close"
          onClick={(e) => { e.stopPropagation(); e.preventDefault(); onClose() }}
          title={session.alive ? 'Kill session' : 'Dismiss'}
        >
          ×
        </button>
      )}
    </a>
  )
}
