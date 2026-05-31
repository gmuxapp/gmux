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
  unreadCount, localHostLabel,
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
    <svg class="shrink-0 w-2.5 h-2.5 mt-[3px] text-text-muted opacity-80" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round">
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
    'session-item group flex items-center py-1 px-1 ps-3.5 cursor-pointer gap-2 transition-colors duration-100 relative no-underline text-inherit',
    selected ? 'selected' : '',
    dragging ? 'session-dragging' : '',
    dropTarget ? 'session-drop-target' : '',
    unavailable ? 'opacity-[0.55]' : '',
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
        ? <svg class="shrink-0 w-[9px] h-[9px] mt-1 text-text-muted opacity-70" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"><title>Peer unavailable</title><path d="M2 2 L10 10 M10 2 L2 10" /></svg>
        : sleeping
        ? <svg class="shrink-0 w-[7px] h-[11px] overflow-visible mt-[3px] text-text-muted opacity-40" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><title>Resumable</title><path d="M7 1h4l-4 4h4" /><path d="M1 5h5l-5 6h5" /></svg>
        : <span class={`session-dot-indicator ${dotState}${arrival ? ` ${arrival}` : ''}`} />
      }
      {showHostMarker && session.peer && <DevcontainerMarker peer={session.peer} />}
      <div class="flex-1 min-w-0 flex flex-col gap-[1px]">
        <div class="flex items-baseline gap-[5px] min-w-0">
          <span class="text-sm font-normal text-text overflow-hidden text-ellipsis whitespace-nowrap leading-[1.35] flex-1 min-w-0">{session.title}</span>
        </div>
        {session.status?.label && (
          <div class="flex items-center gap-1.5 text-[12px] text-text-muted leading-[1.3]">
            <span class="text-text-secondary">{session.status.label}</span>
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
      <div class={`folder-header flex items-center py-1 px-3 ps-3.5 select-none gap-1.5 cursor-pointer transition-colors duration-100${isCurrent ? ' bg-bg-selected' : ''}`}>
        <a
          class={`text-[12px] font-semibold text-text flex-1 overflow-hidden text-ellipsis whitespace-nowrap no-underline${folder.missing ? ' opacity-50' : ''}`}
          href={href}
          title={folder.missing
            ? `${folder.name} no longer exists on ${folder.peer}; remove via the home page`
            : `Open ${folder.name} hub`}
          onClick={onClick}
        >
          {folder.name}
          <HostSuffix peer={folder.peer ?? localHostLabel.value} local={!folder.peer} />
          {folder.missing && <span class="inline-flex items-center justify-center w-3.5 h-3.5 ml-1.5 rounded-full bg-[oklch(25%_0.04_30)] text-[oklch(72%_0.12_30)] text-[10px] font-bold leading-[1cap]" title="Project missing on peer">?</span>}
        </a>
        <LaunchButton
          sessions={folder.sessions}
          selectedId={selId}
          fallbackCwd={folder.launchCwd ?? ''}
          peer={folder.peer}
          className="folder-launch-btn"
        />
      </div>
      <div class="overflow-hidden pt-[1px]">
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
        <div class="sidebar-header flex items-center h-[var(--header-height)] py-0 px-1.5 ps-3.5 gap-2.5 shrink-0 border-b border-border">
          <a
            class={`sidebar-logo font-[Instrument_Sans] text-lg font-bold tracking-[-0.04em] text-text leading-none no-underline py-1 px-2.5 pb-1.5 -my-1.5 -mx-2.5 cursor-pointer rounded transition-colors duration-150 hover:text-white hover:shadow-[0_0_6px_rgba(255,255,255,0.25)]${waiting ? ' bg-waiting' : ''}${bgArrival ? ` bg-${bgArrival}` : ''}`}
            href="/"
            onClick={onClose}
          >gmux</a>
          <button
            class="ml-auto flex items-center justify-center w-7 h-7 p-0 border-0 bg-transparent text-text-muted rounded cursor-pointer transition-colors duration-150 hover:text-text hover:bg-bg-hover"
            onClick={onOpenSettings}
            aria-label="Settings"
            title="Settings"
          >
            <IconSettings />
          </button>
        </div>
        <div class="sidebar-scroll flex-1 overflow-y-auto overflow-x-hidden py-1.5 pb-12 flex flex-col">
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
            <div class="flex justify-center py-4 ps-3.5 pb-1">
              <LaunchButton
                className="sidebar-launch-btn"
                beforeLaunch={seedHomeProject}
                onLaunch={onClose}
              />
            </div>
          )}
          {connected && totalVisible === 0 && !hasProjects && (
            <div class="py-3 px-4 text-[12px] text-text-muted leading-[1.4]">
              Click <strong class="text-text-secondary">+</strong> to start your first session.
            </div>
          )}
          {connected && isOnlyHomeProject && totalVisible > 0 && (
            <div class="py-3 px-4 text-[12px] text-text-muted leading-[1.4]">
              <button class="bg-transparent border-0 text-accent font-[inherit] p-0 cursor-pointer underline underline-offset-[2px] hover:text-text" onClick={onOpenSettings}>
                Add a project
              </button> to organize sessions by repo.
            </div>
          )}
        </div>
      </aside>
    </>
  )
}
