/**
 * Sidebar: project folders, session items, and the navigation shell.
 *
 * Reads shared state directly from the store (signals). Only action
 * callbacks and the mobile open/close toggle are passed as props.
 */

import { useState, useCallback } from 'preact/hooks'
import { sessionPath } from './routing'
import { reorderKeysForFolder } from './projects'
import { LaunchButton } from './launcher'
import { useArrivalPulse } from './use-arrival-pulse'
import {
  folders, selectedId, currentProjectKey,
  activityMap, projects, connState,
  updateProjects, reorderSessions,
  peerStatusByName, isSessionUnavailable, localPeerNames, sessionDotState,
  unreadCount, localHostLabel, unresolvedHosts,
  type DotState,
} from './store'
import { HostSuffix } from './host-suffix'
import type { Session, Folder } from './types'

// ── Types ──

export type NotifPermission = 'default' | 'granted' | 'denied' | 'unavailable'

// Re-export DotState so existing imports keep working.
export type { DotState }

// ── Helpers ──

/** Determine the dot indicator state for a session. */

const bellStroke = { fill: 'none', stroke: 'currentColor', 'stroke-width': '1.4', 'stroke-linecap': 'round' as const, 'stroke-linejoin': 'round' as const }

export const IconBell = ({ muted }: { muted?: boolean }) => (
  <svg viewBox="0 0 14 14" width="14" height="14" {...bellStroke} style={{ opacity: muted ? 0.4 : 1 }}>
    <path d="M7 2a4 4 0 0 1 4 4v2.5l1 1.5H2l1-1.5V6a4 4 0 0 1 4-4Z"/>
    <path d="M5.5 11.5a1.5 1.5 0 0 0 3 0" stroke-width="1.2"/>
  </svg>
)

export const IconSettings = () => (
  <svg viewBox="0 0 16 16" width="15" height="15" {...bellStroke}>
    <path d="M2 4.5h7M12 4.5h2M2 11.5h2M7 11.5h7"/>
    <circle cx="10.5" cy="4.5" r="1.7"/>
    <circle cx="5.5" cy="11.5" r="1.7"/>
  </svg>
)

// ── Drag helpers ──

/** True on devices with a pointer (mouse/trackpad). Touch-only devices
 *  don't support the HTML5 drag API and setting draggable on them
 *  interferes with scroll. */
const canDrag = typeof matchMedia !== 'undefined' && matchMedia('(hover: hover)').matches

interface DragState {
  /** Index of the item being dragged (in the original array). */
  from: number
  /** Current visual insertion target. */
  over: number
}

// ── Components ──

/** Container icon for a devcontainer session inside a mixed-host
 *  folder. Replaces the per-row PeerLabel pill (which didn't tell
 *  anyone "this runs in a container" anyway).
 *
 *  Reachability is guaranteed by the `buildProjectFolders`
 *  bucketing: any session with `.peer` set inside a folder where
 *  `folder.peer === undefined` is a Local peer (parent-owned
 *  devcontainer). Pinned by `projects.test.ts`
 *  ("non-Local peer sessions never land in a locally-owned
 *  folder"). Peer-owned folders are single-host by construction,
 *  so showHostMarker never fires there.
 */
function DevcontainerMarker({ peer }: { peer: string }) {
  return (
    <svg class="session-container-icon" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round">
      <title>{`devcontainer: ${peer}`}</title>
      <rect x="1.5" y="3.5" width="9" height="6" rx="0.5" />
      <path d="M4 3.5v6 M6 3.5v6 M8 3.5v6" />
    </svg>
  )
}

function reorder<T>(arr: T[], from: number, to: number): T[] {
  const next = [...arr]
  const [item] = next.splice(from, 1)
  next.splice(to, 0, item)
  return next
}

function SessionItem({
  session,
  href,
  selected,
  resuming,
  dotState: rawDotState,
  dragging,
  dropTarget,
  unavailable,
  showHostMarker,
  onClose,
  onClick,
  onDragStart,
  onDragOver,
  onDragEnd,
}: {
  session: Session
  href: string
  selected: boolean
  resuming?: boolean
  dotState: DotState
  dragging?: boolean
  dropTarget?: boolean
  /** Session lives on a peer we can't reach right now. */
  unavailable?: boolean
  /** Folder spans multiple hosts; render this session's host marker. */
  showHostMarker?: boolean
  onClose?: () => void
  /** Extra side-effects on click (e.g. close mobile sidebar). */
  onClick?: () => void
  onDragStart?: () => void
  onDragOver?: () => void
  onDragEnd?: () => void
}) {
  const effectiveDotState = resuming ? 'working' : rawDotState
  // Nothing is "unread" if you're already looking at it.
  const dotState = (selected && (effectiveDotState === 'error' || effectiveDotState === 'unread')) ? 'none' : effectiveDotState
  const arrival = useArrivalPulse(dotState)
  const sleeping = !session.alive && session.resumable

  const cls = [
    'session-item',
    selected ? 'selected' : '',
    dragging ? 'session-dragging' : '',
    dropTarget ? 'session-drop-target' : '',
    unavailable ? 'unavailable' : '',
  ].filter(Boolean).join(' ')

  return (
    <a
      class={cls}
      href={href}
      draggable={canDrag && !!onDragStart}
      onClick={() => {
        onClick?.()
      }}
      onAuxClick={(e) => { if (e.button === 1 && onClose) { e.preventDefault(); onClose() } }}
      onDragStart={(e) => {
        e.dataTransfer!.effectAllowed = 'move'
        e.dataTransfer!.setData('text/plain', '')
        onDragStart?.()
      }}
      onDragOver={(e) => { e.preventDefault(); e.dataTransfer!.dropEffect = 'move'; onDragOver?.() }}
      onDrop={(e) => { e.preventDefault(); onDragEnd?.() }}
      onDragEnd={onDragEnd}
    >
      {unavailable
        ? <svg class="session-unavailable-icon" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"><title>Peer unavailable</title><path d="M2 2 L10 10 M10 2 L2 10" /></svg>
        : sleeping
        ? <svg class="session-sleep-icon" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><title>Resumable</title><path d="M7 1h4l-4 4h4" /><path d="M1 5h5l-5 6h5" /></svg>
        : <span class={`session-dot-indicator ${dotState}${arrival ? ` ${arrival}` : ''}`} />
      }
      {showHostMarker && session.peer && <DevcontainerMarker peer={session.peer} />}
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
  currentKey,
  resumingId,
  am,
  peerStatus,
  onCloseSession,
  onClick,
}: {
  folder: Folder
  selId: string | null
  currentKey: string | null
  resumingId: string | null
  am: ReadonlyMap<string, 'active' | 'fading'>
  peerStatus: ReadonlyMap<string, string>
  onCloseSession: (session: Session) => void
  onClick?: () => void
}) {
  const [drag, setDrag] = useState<DragState | null>(null)

  const handleDragStart = useCallback((idx: number) => {
    setDrag({ from: idx, over: idx })
  }, [])

  const handleDragOver = useCallback((idx: number) => {
    setDrag(prev => prev ? { ...prev, over: idx } : null)
  }, [])

  const handleDragEnd = useCallback((visible: Session[]) => {
    if (!drag || drag.from === drag.over) {
      setDrag(null)
      return
    }
    const reordered = reorder(visible, drag.from, drag.over)
    // reorderKeysForFolder partitions sessions by the folder owner's
    // identity and keys them appropriately for the owning daemon's
    // projects.json (namespaced ids for Local-peer sessions inside a
    // parent's local folder, plain ids for everything else). See its
    // docstring for the full routing matrix.
    const visibleKeys = reorderKeysForFolder(
      reordered,
      folder.peer,
      (name) => localPeerNames.value.has(name),
    )
    if (visibleKeys.length > 0) {
      reorderSessions(folder.slug, visibleKeys, folder.peer)
    }
    setDrag(null)
  }, [drag, folder.slug, folder.peer])

  const visible = folder.sessions.filter(s => s.alive || s.resumable)
  const displayItems = drag ? reorder(visible, drag.from, drag.over) : visible
  const isCurrent = currentKey === folder.key
  const href = folder.peer ? `/@${folder.peer}/${folder.slug}` : `/${folder.slug}`
  // Folder spans multiple hosts iff its sessions don't all share the
  // same .peer value. In practice this is the devcontainer case: a
  // local project's folder containing both parent-local sessions
  // (peer=undefined) and Local-peer container sessions. When all
  // sessions agree, per-row markers are noise.
  const folderPeers = new Set(visible.map(s => s.peer ?? ''))
  const mixedHosts = folderPeers.size > 1
  return (
    <div class="folder">
      <div class="folder-header">
        <a
          class={`folder-name${isCurrent ? ' current' : ''}${folder.missing ? ' missing' : ''}${folder.unresolved ? ' unresolved' : ''}`}
          href={href}
          title={folder.unresolved
            ? `Host “${folder.peer}” isn't a connected or manually-added host — it may have been renamed or removed. Open Settings → Hosts to remap or remove it.`
            : folder.missing
            ? `${folder.name} no longer exists on ${folder.peer}; remove via the home page`
            : `Open ${folder.name} hub`}
          onClick={onClick}
        >
          {folder.name}
          <HostSuffix peer={folder.peer ?? localHostLabel.value} local={!folder.peer} />
          {folder.missing && <span class="folder-missing-icon" title="Project missing on peer">?</span>}
          {folder.unresolved && (
            <span class="folder-unresolved-icon" title="Host not found — fix in Settings → Hosts">!</span>
          )}
        </a>
        {!folder.unresolved && (
          <LaunchButton
            sessions={folder.sessions}
            selectedId={selId}
            fallbackCwd={folder.launchCwd ?? ''}
            peer={folder.peer}
            className="folder-launch-btn"
          />
        )}
      </div>
      <div class="folder-sessions">
        {displayItems.map((s, i) => (
          <SessionItem
            key={s.id}
            session={s}
            href={sessionPath(folder.slug, s, folder.peer)}
            selected={selId === s.id}
            resuming={resumingId === s.id}
            dotState={sessionDotState(s, am)}
            unavailable={isSessionUnavailable(s, peerStatus)}
            showHostMarker={mixedHosts}
            dragging={drag !== null && s.id === visible[drag.from]?.id}
            dropTarget={drag !== null && drag.over === i && drag.from !== i}
            onClose={() => onCloseSession(s)}
            onClick={onClick}
            onDragStart={() => handleDragStart(i)}
            onDragOver={() => handleDragOver(i)}
            onDragEnd={() => handleDragEnd(visible)}
          />
        ))}
      </div>
    </div>
  )
}

export function Sidebar({
  resumingId,
  onCloseSession,
  onOpenSettings,
  open,
  onClose,
}: {
  resumingId: string | null
  onCloseSession: (session: Session) => void
  onOpenSettings: () => void
  open: boolean
  onClose: () => void
}) {
  // Read signals; component re-renders only when these values change.
  const foldersVal = folders.value
  const projectsVal = projects.value
  const selId = selectedId.value
  const curKey = currentProjectKey.value
  const am = activityMap.value
  const peerStatus = peerStatusByName.value

  // Waiting indicator on the logo: mirrors the mobile hamburger badge so
  // the always-visible brand mark doubles as a "a session elsewhere is
  // waiting on you" cue. Only the waiting (unread) state is surfaced —
  // working/active are deliberately omitted. unreadCount excludes the
  // selected session (see store.ts); its value also drives the re-blink
  // when an additional session enters the waiting state.
  const waitingCount = unreadCount.value
  const waiting = waitingCount > 0
  // A reference points at a host that's in no roster bucket (renamed /
  // removed): flag the gear so the user knows where the fix lives. (refs #270)
  const hasUnresolved = unresolvedHosts.value.length > 0
  const bgArrival = useArrivalPulse(waiting ? 'unread' : 'none', waitingCount)

  const totalVisible = foldersVal.reduce(
    (n, f) => n + f.sessions.filter(s => s.alive || s.resumable).length, 0,
  )
  const connected = connState.value === 'connected'
  const hasProjects = projectsVal.length > 0
  const isOnlyHomeProject = projectsVal.length === 1
    && projectsVal[0].slug === 'home'
    && !!projectsVal[0].match?.some(r => r.path === '~' && r.exact)

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
            class={`sidebar-logo${waiting ? ' bg-waiting' : ''}${bgArrival ? ` bg-${bgArrival}` : ''}`}
            href="/"
            onClick={onClose}
          >gmux</a>
          <button
            class="sidebar-settings-btn"
            onClick={onOpenSettings}
            aria-label={hasUnresolved ? 'Settings (a referenced host needs attention)' : 'Settings'}
            title={hasUnresolved ? 'A referenced host was not found — open Settings → Hosts' : 'Settings'}
          >
            <IconSettings />
            {hasUnresolved && <span class="settings-attention-pip" aria-hidden="true" />}
          </button>
        </div>
        <div class="sidebar-scroll">
          {foldersVal.map(f => (
            <FolderGroup
              key={f.key}
              folder={f}
              selId={selId}
              currentKey={curKey}
              resumingId={resumingId}
              am={am}
              peerStatus={peerStatus}
              onCloseSession={onCloseSession}
              onClick={onClose}
            />
          ))}
          {connected && !hasProjects && (
            <div class="sidebar-empty-launch">
              <LaunchButton
                className="sidebar-launch-btn"
                beforeLaunch={seedHomeProject}
                onLaunch={onClose}
              />
            </div>
          )}
          {connected && totalVisible === 0 && !hasProjects && (
            <div class="sidebar-hint">
              Click <strong>+</strong> to start your first session.
            </div>
          )}
          {connected && isOnlyHomeProject && totalVisible > 0 && (
            <div class="sidebar-hint">
              <button class="sidebar-hint-link" onClick={onOpenSettings}>
                Add a project
              </button> to organize sessions by repo.
            </div>
          )}
        </div>
      </aside>
    </>
  )
}
