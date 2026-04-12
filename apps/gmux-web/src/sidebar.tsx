/**
 * Sidebar: project folders, session items, and the navigation shell.
 *
 * Reads shared state directly from the store (signals). Only action
 * callbacks and the mobile open/close toggle are passed as props.
 */

import { sessionPath } from './routing'
import { LaunchButton } from './launcher'
import { useArrivalPulse } from './use-arrival-pulse'
import {
  folders, selectedId, currentProjectSlug,
  activityMap, unmatchedActiveCount, projects, connState,
  updateProjects, peerAppearance,
  type DotState,
} from './store'
import type { Session, Folder } from './types'

// ── Types ──

export type NotifPermission = 'default' | 'granted' | 'denied' | 'unavailable'

// Re-export DotState so existing imports keep working.
export type { DotState }

// ── Helpers ──

/** Determine the dot indicator state for a session. */
function sessionDotState(session: Session, am: ReadonlyMap<string, 'active' | 'fading'>): DotState {
  if (session.alive && session.status?.error)   return 'error'
  if (session.alive && session.status?.working) return 'working'
  if (session.unread) return 'unread'
  const act = am.get(session.id)
  if (act === 'active') return 'active'
  if (act === 'fading') return 'fading'
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
  dotState: rawDotState,
  onResume,
  onClose,
  onClick,
}: {
  session: Session
  href: string
  selected: boolean
  resuming?: boolean
  dotState: DotState
  onResume?: (id: string) => void
  onClose?: () => void
  /** Extra side-effects on click (e.g. close mobile sidebar). */
  onClick?: () => void
}) {
  const effectiveDotState = resuming ? 'working' : rawDotState
  // Nothing is "unread" if you're already looking at it.
  const dotState = (selected && (effectiveDotState === 'error' || effectiveDotState === 'unread')) ? 'none' : effectiveDotState
  const arrival = useArrivalPulse(dotState)
  const sleeping = !session.alive && session.resumable

  return (
    <a
      class={`session-item ${selected ? 'selected' : ''}`}
      href={href}
      onClick={(e) => {
        onClick?.()
        if (sleeping) {
          e.preventDefault()
          onResume?.(session.id)
        }
      }}
      onAuxClick={(e) => { if (e.button === 1 && onClose) { e.preventDefault(); onClose() } }}
    >
      {sleeping
        ? <svg class="session-sleep-icon" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><title>Resumable</title><path d="M7 1h4l-4 4h4" /><path d="M1 5h5l-5 6h5" /></svg>
        : <span class={`session-dot-indicator ${dotState}${arrival ? ` ${arrival}` : ''}`} />
      }
      {session.peer && (() => {
        const a = peerAppearance.value.get(session.peer)
        return <span class="session-peer-label" title={session.peer} style={a && { color: a.color, background: a.bg }}>{a?.label ?? session.peer[0].toUpperCase()}</span>
      })()}
      <div class="session-content">
        <div class="session-title-row">
          <span class="session-title">{session.title}</span>
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
  selId,
  curProjectSlug,
  resumingId,
  am,
  onResume,
  onCloseSession,
  onClick,
}: {
  folder: Folder
  selId: string | null
  curProjectSlug: string | null
  resumingId: string | null
  am: ReadonlyMap<string, 'active' | 'fading'>
  onResume: (id: string) => void
  onCloseSession: (session: Session) => void
  onClick?: () => void
}) {
  const visible = folder.sessions.filter(s => s.alive || s.resumable)
  const isCurrent = curProjectSlug === folder.path
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
        <LaunchButton
          sessions={folder.sessions}
          selectedId={selId}
          fallbackCwd={folder.launchCwd ?? ''}
          className="folder-launch-btn"
        />
      </div>
      <div class="folder-sessions">
        {visible.map(s => (
          <SessionItem
            key={s.id}
            session={s}
            href={sessionPath(folder.path, s)}
            selected={selId === s.id}
            resuming={resumingId === s.id}
            dotState={sessionDotState(s, am)}
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
  resumingId,
  onResume,
  onCloseSession,
  onManageProjects,
  open,
  onClose,
  notifPermission,
  onRequestNotifPermission,
}: {
  resumingId: string | null
  onResume: (id: string) => void
  onCloseSession: (session: Session) => void
  onManageProjects: () => void
  open: boolean
  onClose: () => void
  notifPermission: NotifPermission
  onRequestNotifPermission: () => void
}) {
  // Read signals; component re-renders only when these values change.
  const foldersVal = folders.value
  const selId = selectedId.value
  const curProjectSlug = currentProjectSlug.value
  const unmatchedCount = unmatchedActiveCount.value
  const am = activityMap.value

  const totalVisible = foldersVal.reduce(
    (n, f) => n + f.sessions.filter(s => s.alive || s.resumable).length, 0,
  )
  const projectsVal = projects.value
  const connected = connState.value === 'connected'
  const hasProjects = projectsVal.length > 0
  const isOnlyHomeProject = projectsVal.length === 1
    && projectsVal[0].slug === 'home'
    && projectsVal[0].match.some(r => r.path === '~' && r.exact)

  const seedHomeProject = async () => {
    if (projects.value.length === 0) {
      await updateProjects([{ slug: 'home', match: [{ path: '~', exact: true }] }])
    }
  }

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
          {connected && !hasProjects && (
            <LaunchButton
              className="sidebar-launch-btn"
              beforeLaunch={seedHomeProject}
              onLaunch={onClose}
            />
          )}
        </div>
        <div class="sidebar-scroll">
          {foldersVal.map(f => (
            <FolderGroup
              key={f.path}
              folder={f}
              selId={selId}
              curProjectSlug={curProjectSlug}
              resumingId={resumingId}
              am={am}
              onResume={onResume}
              onCloseSession={onCloseSession}
              onClick={onClose}
            />
          ))}
          {connected && totalVisible === 0 && !hasProjects && (
            <div class="sidebar-hint">
              Click <strong>+</strong> to start your first session.
            </div>
          )}
          {connected && isOnlyHomeProject && totalVisible > 0 && (
            <div class="sidebar-hint">
              <button class="sidebar-hint-link" onClick={onManageProjects}>
                Manage projects
              </button> to organize sessions by repo.
            </div>
          )}
        </div>
        <div class="sidebar-footer">
          <button class="manage-projects-btn" onClick={onManageProjects}>
            Manage projects
            {unmatchedCount > 0 && (
              <span class="manage-projects-badge">{unmatchedCount}</span>
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
