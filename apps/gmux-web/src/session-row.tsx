// Single-line session row for the activity feed (home dashboard + the
// sidebar's Activity view). Reads `project · title`, with the project
// leading as muted context and optional host / cwd / status trailing.
// No per-row time: the day headings carry recency and the order within
// a day says the rest, so a timestamp would just duplicate the heading
// (and used to contradict it). Shared dot-state / unavailability logic
// via store helpers.
//
// Kept deliberately separate from the sidebar's Projects-view
// SessionItem rather than unified behind a density prop: that variant
// is drag-aware and folder-scoped; a single component would accumulate
// flags faster than it would save code.

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
  /** Show the project name on line 2. */
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

  // Single line, in reading order: project on host · title · cwd ·
  // status. Project leads as muted context; the title is the anchor.
  // Only requested segments render; empty ones add no separator.
  const segments: preact.ComponentChildren[] = []
  const hostEl = showHost && session.peer
    ? <HostSuffix peer={session.peer} connective="on" />
    : null
  if (showProject && projectName) {
    // Grouped so the host joins the project with "on" and no dot between.
    segments.push(
      <span class="session-row-project">{projectName}{hostEl && <> {hostEl}</>}</span>,
    )
  } else if (hostEl) {
    segments.push(hostEl)
  }
  segments.push(<span class="session-row-title">{session.title}</span>)
  if (showCwd && cwdLabel) {
    // Truncated in CSS; the title carries the full absolute cwd so a
    // hover/long-press reveals the path the relative label abbreviates.
    segments.push(<span class="session-row-cwd" title={session.cwd || cwdLabel}>{cwdLabel}</span>)
  }
  if (statusText) {
    segments.push(<span class="session-row-status">{statusText}</span>)
  }
  // Same conversation file open in another live runner (ADR 0011 N:1).
  if (session.conversation_file && duplicateConversationFiles.value.has(session.conversation_file)) {
    segments.push(
      <span class="session-row-dup" title="This conversation is open in more than one tab">⚠ open elsewhere</span>,
    )
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
        {segments.map((seg, i) => (
          // Long-form Fragment carries a key: the shorthand `<>` can't,
          // and without one Preact falls back to index diffing across
          // renders. The content is positional (segment N is whatever
          // segment N happens to be this render), so index is honest.
          <Fragment key={i}>
            {i > 0 && <span class="session-row-sep"> · </span>}
            {seg}
          </Fragment>
        ))}
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
