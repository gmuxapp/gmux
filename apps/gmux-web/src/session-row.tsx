// Session row for the activity feed (home dashboard + the sidebar's
// Activity view). Two layouts, chosen by the caller per day bucket:
//
//   - full (compact=false): today's sessions. Two lines — title, then
//     "age · project on host · cwd". The age is worth the room for the
//     things you're actively working on.
//   - compact (compact=true): older buckets. One line, "project · title",
//     project leading as muted context. No age: the day heading (and the
//     order within it) already carry recency, so a per-row time would
//     duplicate — and used to contradict — the heading.
//
// Dead sessions never surface an "exited (N)" label here: the sleep /
// dot indicator already conveys the state, and the text was noise in a
// list. Shared dot-state / unavailability logic via store helpers.
//
// Kept deliberately separate from the sidebar's Projects-view
// SessionItem rather than unified behind more flags: that variant is
// drag-aware and folder-scoped.

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
  /** Single-line `project · title` layout (older day buckets). When
   *  false/omitted: the two-line layout with a relative age (today). */
  compact?: boolean
  /** Show the project name. */
  showProject?: boolean
  /** Project display name to render when showProject is true. */
  projectName?: string
  /** Show the owning peer's host suffix next to the project. */
  showHost?: boolean
  /** Show the session's cwd. */
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

/** Compact "Nm" / "Nh" / "Nd" relative-time for the full row's age.
 *  Sub-minute collapses to "now" so a fresh row doesn't tick every
 *  second. */
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
  return `${Math.floor(hours / 24)}d`
}

/** Middle-truncate a project name so a long one keeps its head and tail
 *  (…) instead of vanishing behind a flex ellipsis — you can still tell
 *  `review-coordinator` from `review-controller`. Keeps the first
 *  `head` and last `tail` chars (10 + … + 5 = 16 by default); names
 *  within that budget are untouched. Counts by code point (spread, not
 *  .slice) so an emoji or other non-BMP char at a cut point can't be
 *  split into a lone surrogate (�). */
export function middleTruncate(s: string, head = 10, tail = 5): string {
  const cp = [...s]
  if (cp.length <= head + tail + 1) return s
  return `${cp.slice(0, head).join('')}…${cp.slice(cp.length - tail).join('')}`
}

export function SessionRow({
  session,
  href,
  selected,
  resuming,
  compact,
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
  // Selection mutes attention-grabbing dots (mirrors sidebar behavior):
  // if you're already looking at it, "unread" / "error" aren't useful.
  const dot = (selected && (rawDot === 'error' || rawDot === 'unread')) ? 'none' : rawDot
  const arrival = useArrivalPulse(dot)

  const cls = [
    'session-row',
    compact ? 'session-row-compact' : 'session-row-full',
    selected ? 'selected' : '',
    unavailable ? 'unavailable' : '',
  ].filter(Boolean).join(' ')

  const hostEl = showHost && session.peer
    ? <HostSuffix peer={session.peer} connective="on" />
    : null
  // Project (middle-truncated) with the host grouped on via "on" and no
  // dot between. Falls back to a host-only segment when no project.
  let projectSeg: preact.ComponentChildren = null
  if (showProject && projectName) {
    const shown = middleTruncate(projectName)
    projectSeg = (
      <span class="session-row-project" title={shown !== projectName ? projectName : undefined}>
        {shown}{hostEl && <> {hostEl}</>}
      </span>
    )
  } else if (hostEl) {
    projectSeg = hostEl
  }
  const titleSeg = <span class="session-row-title">{session.title}</span>
  const cwdSeg = showCwd && cwdLabel
    // Truncated in CSS; the title carries the full absolute cwd so a
    // hover/long-press reveals the path the relative label abbreviates.
    ? <span class="session-row-cwd" title={session.cwd || cwdLabel}>{cwdLabel}</span>
    : null
  // Same conversation file open in another live runner (ADR 0011 N:1).
  const dupSeg = session.conversation_file && duplicateConversationFiles.value.has(session.conversation_file)
    ? <span class="session-row-dup" title="This conversation is open in more than one tab">⚠ open elsewhere</span>
    : null

  const withSeps = (segs: preact.ComponentChildren[]) =>
    segs.filter(Boolean).map((seg, i) => (
      // Long-form Fragment carries a key; content is positional so index
      // is the honest key.
      <Fragment key={i}>
        {i > 0 && <span class="session-row-sep"> · </span>}
        {seg}
      </Fragment>
    ))

  const content = compact
    ? <div class="session-row-content">{withSeps([projectSeg, titleSeg, cwdSeg, dupSeg])}</div>
    : (() => {
        const age = formatAge(session.last_output_at ?? session.created_at, Date.now())
        const meta = withSeps([
          age ? <span class="session-row-age">{age}</span> : null,
          projectSeg, cwdSeg, dupSeg,
        ])
        return (
          <div class="session-row-content">
            {titleSeg}
            {meta.length > 0 && <div class="session-row-meta">{meta}</div>}
          </div>
        )
      })()

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
      {content}
      {onClose && (
        <button
          class="session-close-btn"
          onClick={(e) => { e.stopPropagation(); e.preventDefault(); onClose() }}
          title={session.alive ? 'Kill session' : 'Dismiss'}
        >
          ×
        </button>
      )}
    </a>
  )
}
