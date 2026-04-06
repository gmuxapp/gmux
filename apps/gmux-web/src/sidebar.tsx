/**
 * Sidebar: project folders, session items, and the navigation shell.
 *
 * Pure presentational components; no data fetching. All data and callbacks
 * are passed in via props from App.
 */

import type { Session, Folder } from './types'
import { sessionPath } from './types'
import { LaunchButton } from './launcher'
import { useArrivalPulse } from './use-arrival-pulse'
import type { HealthData } from './use-session-data'

// ── Types ──

export type NotifPermission = 'default' | 'granted' | 'denied' | 'unavailable'

export type DotState = 'working' | 'error' | 'unread' | 'active' | 'fading' | 'none'

// ── Helpers ──

/** Determine the dot indicator state for a session. */
function sessionDotState(session: Session, isActive?: boolean, isFading?: boolean): DotState {
  if (session.alive && session.status?.error)   return 'error'
  if (session.alive && session.status?.working) return 'working'
  if (session.unread) return 'unread'
  if (isActive) return 'active'
  if (isFading) return 'fading'
  return 'none'
}

const bellStroke = { fill: 'none', stroke: 'currentColor', 'stroke-width': '1.4', 'stroke-linecap': 'round' as const, 'stroke-linejoin': 'round' as const }

export const IconBell = ({ muted }: { muted?: boolean }) => (
  <svg viewBox="0 0 14 14" width="14" height="14" {...bellStroke} style={{ opacity: muted ? 0.4 : 1 }}>
    <path d="M7 2a4 4 0 0 1 4 4v2.5l1 1.5H2l1-1.5V6a4 4 0 0 1 4-4Z"/>
    <path d="M5.5 11.5a1.5 1.5 0 0 0 3 0" stroke-width="1.2"/>
  </svg>
)

// ── Components ──

function SessionItem({
  session,
  href,
  selected,
  resuming,
  isActive,
  isFading,
  onResume,
  onClose,
  onClick,
}: {
  session: Session
  href: string
  selected: boolean
  resuming?: boolean
  isActive?: boolean
  isFading?: boolean
  onResume?: (id: string) => void
  onClose?: () => void
  /** Extra side-effects on click (e.g. close mobile sidebar). */
  onClick?: () => void
}) {
  const rawDotState = resuming ? 'working' : sessionDotState(session, isActive, isFading)
  // Nothing is "unread" if you're already looking at it.
  const dotState = (selected && (rawDotState === 'error' || rawDotState === 'unread')) ? 'none' : rawDotState
  const arrival = useArrivalPulse(dotState)
  const sleeping = !session.alive && session.resumable

  return (
    <a
      class={`session-item ${selected ? 'selected' : ''}`}
      href={href}
      onClick={(e) => {
        onClick?.()
        if (sleeping) {
          // Resumable: don't navigate yet; trigger resume and let the
          // auto-select effect navigate once the session comes alive.
          e.preventDefault()
          onResume?.(session.id)
        }
        // Alive sessions: let the click propagate to preact-iso's
        // document click handler, which does client-side navigation.
      }}
      onAuxClick={(e) => { if (e.button === 1 && onClose) { e.preventDefault(); onClose() } }}
    >
      {sleeping
        ? <svg class="session-sleep-icon" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><title>Resumable</title><path d="M7 1h4l-4 4h4" /><path d="M1 5h5l-5 6h5" /></svg>
        : <span class={`session-dot-indicator ${dotState}${arrival ? ` ${arrival}` : ''}`} />
      }
      <div class="session-content">
        <div class="session-title-row">
          <span class="session-title">{session.title}</span>
          {session.peer && (
            <span class={`session-peer-pill peer-${session.peer.split('/')[0]}`}>{session.peer}</span>
          )}
        </div>
        {session.status?.label && (
          <div class="session-meta">
            <span class="session-status-label">{session.status.label}</span>
          </div>
        )}
      </div>
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

function FolderGroup({
  folder,
  selectedId,
  currentProjectSlug,
  resumingId,
  isSessionActive,
  isSessionFading,
  onResume,
  onCloseSession,
  onClick,
}: {
  folder: Folder
  selectedId: string | null
  currentProjectSlug: string | null
  resumingId: string | null
  isSessionActive: (id: string) => boolean
  isSessionFading: (id: string) => boolean
  onResume: (id: string) => void
  onCloseSession: (session: Session) => void
  /** Extra side-effects on click (e.g. close mobile sidebar). */
  onClick?: () => void
}) {
  // Show alive sessions + resumable sessions that died on their own.
  // Non-resumable dead sessions are filtered out.
  const visible = folder.sessions.filter(s => s.alive || s.resumable)

  const isCurrent = currentProjectSlug === folder.path
  return (
    <div class="folder">
      <div class="folder-header">
        <a
          class={`folder-name${isCurrent ? ' current' : ''}`}
          href={`/${folder.path}`}
          title={`Open ${folder.name} hub`}
          onClick={onClick}
        >
          {folder.name}
        </a>
        <LaunchButton cwd={folder.sessions[0]?.cwd ?? folder.launchCwd} storageKey={folder.path} className="folder-launch-btn" />
      </div>
      <div class="folder-sessions">
        {visible.map(s => (
          <SessionItem
            key={s.id}
            session={s}
            href={sessionPath(folder.path, s)}
            selected={selectedId === s.id}
            resuming={resumingId === s.id}
            isActive={isSessionActive(s.id)}
            isFading={isSessionFading(s.id)}
            onResume={onResume}
            onClose={() => onCloseSession(s)}
            onClick={onClick}
          />
        ))}
      </div>
    </div>
  )
}

export function Sidebar({
  folders,
  unmatchedActiveCount,
  selectedId,
  currentProjectSlug,
  resumingId,
  isSessionActive,
  isSessionFading,
  onResume,
  onCloseSession,
  onManageProjects,
  open,
  onClose,
  health,
  notifPermission,
  onRequestNotifPermission,
}: {
  folders: Folder[]
  unmatchedActiveCount: number
  selectedId: string | null
  currentProjectSlug: string | null
  resumingId: string | null
  isSessionActive: (id: string) => boolean
  isSessionFading: (id: string) => boolean
  onResume: (id: string) => void
  onCloseSession: (session: Session) => void
  onManageProjects: () => void
  open: boolean
  onClose: () => void
  health: HealthData | null
  notifPermission: NotifPermission
  onRequestNotifPermission: () => void
}) {
  const hasProjects = folders.length > 0

  return (
    <>
      <div class={`sidebar-overlay ${open ? 'visible' : ''}`} onClick={onClose} />
      <aside class={`sidebar ${open ? 'open' : ''}`}>
        <div class="sidebar-header">
          <a
            class="sidebar-logo"
            href="/"
            onClick={onClose}
          >gmux</a>
          {health?.version ? (
            <a
              class={`sidebar-badge${health.update_available ? ' sidebar-badge-update' : ''}`}
              href="https://gmux.app/changelog/"
              target="_blank"
              title={health.update_available
                ? 'Update available - safe to update while sessions are running'
                : `gmux ${health.version}`}
            >
              {health.update_available
                ? <>{health.version} &rarr; {health.update_available}</>
                : health.version}
            </a>
          ) : null}
          <LaunchButton className="sidebar-launch-btn" onLaunch={onClose} />
        </div>
        <div class="sidebar-scroll">
          {hasProjects ? folders.map(f => (
            <FolderGroup
              key={f.path}
              folder={f}
              selectedId={selectedId}
              currentProjectSlug={currentProjectSlug}
              resumingId={resumingId}
              isSessionActive={isSessionActive}
              isSessionFading={isSessionFading}
              onResume={onResume}
              onCloseSession={onCloseSession}
              onClick={onClose}
            />
          )) : (
            <div class="sidebar-empty">
              <div class="sidebar-empty-title">No projects yet</div>
              <div class="sidebar-empty-body">
                Projects group your sessions by repository.
              </div>
              <button class="sidebar-empty-action" onClick={onManageProjects}>
                Add a project
              </button>
            </div>
          )}
        </div>
        <div class="sidebar-footer">
          <button class="manage-projects-btn" onClick={onManageProjects}>
            Manage projects
            {unmatchedActiveCount > 0 && (
              <span class="manage-projects-badge">{unmatchedActiveCount}</span>
            )}
          </button>
          {notifPermission === 'default' && (
            <button class="notif-btn" onClick={onRequestNotifPermission}>
              <IconBell /> Enable notifications
            </button>
          )}
          {notifPermission === 'denied' && (
            <div class="notif-denied">
              <IconBell muted /> Notifications blocked in browser settings
            </div>
          )}
        </div>
      </aside>
    </>
  )
}
