/**
 * Sidebar: project folders, session items, and the navigation shell.
 *
 * Reads shared state directly from the store (signals). Only action
 * callbacks and the mobile open/close toggle are passed as props.
 */

import { useState, useCallback, useRef, useEffect } from 'preact/hooks'
import { needsReveal } from './sidebar-reveal'
import { sessionPath } from './routing'
import { selectorLabel, folderMatchesFilter, type Selector } from './tab-filter'
import { reorderKeysForFolder } from './projects'
import { LaunchButton } from './launcher'
import { useArrivalPulse } from './use-arrival-pulse'
import {
  folders, selectedId,
  activityMap, projects, connState, health, peers,
  collapsedFolders, toggleFolderCollapsed,
  updateProjects, reorderSessions,
  peerStatusByName, isSessionUnavailable, localPeerNames, sessionDotState,
  unreadCount, localHostLabel, unresolvedHosts, duplicateConversationFiles,
  sidebarActivity, sidebarMode, setSidebarMode,
  activeSelectors, removeSelector, setHostFilter,
  aliveOnly, setAliveOnly, tabHref,
  type DotState,
} from './store'
import { HostSuffix } from './host-suffix'
import { SessionRow } from './session-row'
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

const IconArrange = () => (
  <svg viewBox="0 0 16 16" width="15" height="15" {...bellStroke}>
    <path d="M2.5 4h8M2.5 8h6M2.5 12h4"/>
    <path d="M13 6.5v6M13 12.5l-2-2M13 12.5l2-2"/>
  </svg>
)

/** Disclosure chevron for folder headers. Points down when expanded;
 *  CSS rotates it to point right when collapsed, so the same glyph
 *  animates between states. */
const IconChevron = ({ className }: { className?: string }) => (
  <svg class={className} viewBox="0 0 12 12" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M2.5 4.5 L6 8 L9.5 4.5" />
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
  // Same conversation file live in another runner (ADR 0011 N:1).
  const duplicateOpen = !!session.conversation_file && duplicateConversationFiles.value.has(session.conversation_file)

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
        {duplicateOpen && (
          <div class="session-meta">
            <span class="session-dup-warning" title="This conversation is open in more than one tab">⚠ open elsewhere</span>
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
  resumingId,
  am,
  peerStatus,
  aliveOnly,
  onCloseSession,
  onClick,
}: {
  folder: Folder
  selId: string | null
  resumingId: string | null
  am: ReadonlyMap<string, 'active' | 'fading'>
  peerStatus: ReadonlyMap<string, string>
  /** Hide dead-but-resumable sessions (tab-scoped toggle). */
  aliveOnly?: boolean
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

  // folder.sessions is already the filtered set (see store.ts
  // sidebarSessions) — alive-only, ?filter=, and the resumable baseline
  // are all applied upstream. Render it as-is.
  const visible = folder.sessions
  const displayItems = drag ? reorder(visible, drag.from, drag.over) : visible
  const collapsed = collapsedFolders.value.has(folder.key)
  // A collapsed folder still shows the selected session: you can't hide
  // the thing you're looking at (it also keeps the row in the DOM for
  // mobile scroll-into-view). The header reads as collapsed; the one
  // row just sits beneath it.
  const shown = collapsed ? displayItems.filter(s => s.id === selId) : displayItems
  // Drag-reorder is disabled while collapsed (the visible subset no
  // longer maps onto the stored order) or under the alive-only toggle.
  const dragDisabled = collapsed || !!aliveOnly
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
        <button
          type="button"
          class={`folder-name${folder.missing ? ' missing' : ''}${folder.unresolved ? ' unresolved' : ''}`}
          aria-expanded={!collapsed}
          title={folder.unresolved
            ? `Host “${folder.peer}” isn't a connected or manually-added host — it may have been renamed or removed. Open Settings → Hosts to remap or remove it.`
            : folder.missing
            ? `${folder.name} no longer exists on ${folder.peer} — remove this reference in Settings → Projects.`
            : collapsed ? `Expand ${folder.name}` : `Collapse ${folder.name}`}
          onClick={() => toggleFolderCollapsed(folder.key)}
        >
          <IconChevron className={`folder-chevron${collapsed ? ' collapsed' : ''}`} />
          <span class="folder-name-label">{folder.name}</span>
          <HostSuffix peer={folder.peer ?? localHostLabel.value} local={!folder.peer} />
          {folder.missing && <span class="folder-missing-icon" title="Project missing on host — remove in Settings → Projects">?</span>}
          {folder.unresolved && (
            <span class="folder-unresolved-icon" title="Host not found — fix in Settings → Hosts">!</span>
          )}
        </button>
        {!folder.unresolved && (
          <LaunchButton
            // Project-row "+" always launches in the project's canonical
            // dir (the first match-rule path, carried by launchCwd), never
            // a recently-used session's cwd. peer stays authoritative for
            // references.
            cwd={folder.launchCwd ?? ''}
            peer={folder.peer}
            className="folder-launch-btn"
          />
        )}
      </div>
      {shown.length > 0 && (
      <div class="folder-sessions">
        {shown.map((s, i) => (
          <SessionItem
            key={s.id}
            session={s}
            href={tabHref(sessionPath(folder.slug, s, folder.peer))}
            selected={selId === s.id}
            resuming={resumingId === s.id}
            dotState={sessionDotState(s, am)}
            unavailable={isSessionUnavailable(s, peerStatus)}
            showHostMarker={mixedHosts}
            dragging={drag !== null && s.id === visible[drag.from]?.id}
            dropTarget={drag !== null && drag.over === i && drag.from !== i}
            onClose={() => onCloseSession(s)}
            onClick={onClick}
            onDragStart={dragDisabled ? undefined : () => handleDragStart(i)}
            onDragOver={dragDisabled ? undefined : () => handleDragOver(i)}
            onDragEnd={dragDisabled ? undefined : () => handleDragEnd(visible)}
          />
        ))}
      </div>
      )}
    </div>
  )
}

/** Compact popover behind the header's arrange icon. Three concerns,
 *  three lifetimes:
 *    View  — Projects/Activity, persisted in the URL (?sidebar=).
 *    Host  — narrows the tab to one host (`*@host` in ?filter=).
 *    Alive only — hides resumable corpses; sessionStorage (per tab).
 *  One entry point, instant switching — the list is the preview. */
function ViewMenu({ open, onToggle }: { open: boolean; onToggle: () => void }) {
  const mode = sidebarMode.value
  const selectors = activeSelectors.value
  // The Host radio reflects a sole `*@host` selector; anything more
  // exotic (project selectors, several hosts) lives in the chip row.
  const hostSelectors = selectors.filter(s => s.project === '*')
  const currentHost = hostSelectors.length === 1 ? hostSelectors[0].host : null
  const alive = aliveOnly.value

  const Option = ({ checked, label, onSelect }: {
    checked: boolean; label: string; onSelect: () => void
  }) => (
    <button
      class={`view-menu-option${checked ? ' active' : ''}`}
      onClick={() => { onSelect(); onToggle() }}
    >
      <span class="view-menu-check">{checked ? '✓' : ''}</span>
      {label}
    </button>
  )

  // Host list: the viewer's own host first, then connected/known peers.
  const localName = health.value?.hostname
  const peerNames = peers.value.map(p => p.name)

  return (
    <div class="view-menu-anchor">
      <button
        class={`sidebar-settings-btn${open ? ' open' : ''}`}
        onClick={onToggle}
        aria-label="List options"
        title="List options"
        aria-expanded={open}
      >
        <IconArrange />
      </button>
      {open && (
        // Transparent backdrop: a click anywhere outside the popover
        // closes it (the sidebar-scroll onClick only covers the list).
        <div class="view-menu-backdrop" onClick={onToggle} />
      )}
      {open && (
        <div class="view-menu" role="menu">
          <div class="view-menu-label">View</div>
          <Option checked={mode === 'projects'} label="Projects" onSelect={() => setSidebarMode('projects')} />
          <Option checked={mode === 'activity'} label="Activity" onSelect={() => setSidebarMode('activity')} />
          <div class="view-menu-label">Host</div>
          <Option checked={currentHost === null && hostSelectors.length === 0} label="All hosts" onSelect={() => setHostFilter(null)} />
          {localName && (
            <Option checked={currentHost === localName || currentHost === 'local'} label={localName} onSelect={() => setHostFilter(localName)} />
          )}
          {peerNames.map(name => (
            <Option key={name} checked={currentHost === name} label={name} onSelect={() => setHostFilter(name)} />
          ))}
          <div class="view-menu-label">Show</div>
          <Option checked={alive} label="Alive only" onSelect={() => setAliveOnly(!alive)} />
        </div>
      )}
    </div>
  )
}

/** Chip row: renders one removable chip per `?filter=` selector.
 *  Occupies zero pixels when the tab isn't narrowed; when it is, the
 *  narrowing is loud enough that nobody wonders where their sessions
 *  went. */
function FilterChips({ selectors }: { selectors: readonly Selector[] }) {
  if (selectors.length === 0) return null
  return (
    <div class="sidebar-chips">
      {selectors.map(sel => (
        <span class="sidebar-chip" key={`${sel.project}@${sel.host}`}>
          {selectorLabel(sel)}
          <button
            class="sidebar-chip-x"
            onClick={() => removeSelector(sel)}
            aria-label={`Remove filter ${selectorLabel(sel)}`}
            title="Remove filter"
          >×</button>
        </span>
      ))}
    </div>
  )
}

/** Activity view: the same sessions as the Projects view (folders),
 *  grouped by activity instead of by project. Flat list — no folder
 *  headers — with section labels (Waiting / Active / recency buckets /
 *  Older) and per-row project context. */
function ActivityList({
  selId,
  resumingId,
  onCloseSession,
  onClick,
}: {
  selId: string | null
  resumingId: string | null
  onCloseSession: (session: Session) => void
  onClick?: () => void
}) {
  const buckets = sidebarActivity.value
  const foldersVal = folders.value

  const folderBySessionId = new Map<string, Folder>()
  for (const f of foldersVal) {
    for (const s of f.sessions) folderBySessionId.set(s.id, f)
  }

  const renderRow = (s: Session) => {
    const folder = folderBySessionId.get(s.id)
    if (!folder) return null
    return (
      <SessionRow
        key={s.id}
        session={s}
        href={tabHref(sessionPath(folder.slug, s, folder.peer))}
        selected={selId === s.id}
        resuming={resumingId === s.id}
        showProject
        projectName={folder.name}
        onClick={onClick}
        onClose={() => onCloseSession(s)}
      />
    )
  }

  // Drop folderless sessions per bucket (the brief post-restart window
  // where recovered sessions arrive unstamped) so a day heading never
  // renders with no rows. partitionByDay never emits empty buckets.
  const sections = buckets
    .map(b => ({ label: b.label, sessions: b.sessions.filter(s => folderBySessionId.has(s.id)) }))
    .filter(sec => sec.sessions.length > 0)

  if (sections.length === 0) {
    return (
      <div class="sidebar-hint">
        {activeSelectors.value.length > 0
          ? 'No sessions match this filter.'
          : 'No sessions yet.'}
      </div>
    )
  }

  return (
    <>
      {sections.map(sec => (
        <div class="sidebar-activity-section" key={sec.label ?? 'today'}>
          {sec.label !== null && <div class="sidebar-section-title">{sec.label}</div>}
          {sec.sessions.map(renderRow)}
        </div>
      ))}
    </>
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
  const am = activityMap.value
  const peerStatus = peerStatusByName.value
  const mode = sidebarMode.value
  const selectors = activeSelectors.value
  const aliveOnlyVal = aliveOnly.value
  const collapsedVal = collapsedFolders.value
  const [menuOpen, setMenuOpen] = useState(false)

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

  // Mobile: when the off-canvas sidebar opens (or the selection changes
  // while it's open), reveal the selected session instead of leaving the
  // user at the top of the list. Scrolls only when the row is actually
  // outside the viewport, and centers it so neighbors give context.
  // Desktop is unaffected: there `open` never transitions to true.
  //
  // No retry/polling: the effect runs after commit, and the selected row
  // is guaranteed present whenever this runs. `open` can only become true
  // once data has loaded (the mobile open-trigger lives in surfaces that
  // don't render until then); the selected session is pinned into the
  // list past any `?filter=` (see store.ts sidebarSessions); and a
  // collapsed folder still renders its selected row (see FolderGroup). So
  // the row is in the DOM by the time this reads it.
  const scrollRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const container = scrollRef.current
    // Both row flavors: .session-item (projects view) and .session-row
    // (activity view).
    const el = container?.querySelector<HTMLElement>('.session-item.selected, .session-row.selected')
    if (!container || !el) return
    if (needsReveal(container.getBoundingClientRect(), el.getBoundingClientRect()))
      el.scrollIntoView({ block: 'center' })
    // Re-reveal when the selected row's placement can shift while the
    // drawer stays open: selection change, Projects<->Activity switch,
    // alive-only toggle, a filter edit, or a folder collapse/expand.
  }, [open, selId, mode, aliveOnlyVal, selectors, collapsedVal])

  // The view menu shouldn't outlive the sidebar on mobile.
  useEffect(() => { if (!open) setMenuOpen(false) }, [open])

  // folder.sessions is already the shown set (see store.ts
  // sidebarSessions), so this is just the visible session count.
  const totalVisible = foldersVal.reduce((n, f) => n + f.sessions.length, 0)
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
            href={tabHref('/')}
            onClick={onClose}
          >gmux</a>
          <ViewMenu open={menuOpen} onToggle={() => setMenuOpen(v => !v)} />
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
        <FilterChips selectors={selectors} />
        <div class="sidebar-scroll" ref={scrollRef} onClick={() => menuOpen && setMenuOpen(false)}>
          {mode === 'projects' && selectors.length > 0
            && foldersVal.every(f => f.sessions.length === 0
              && !folderMatchesFilter(f, selectors, health.value?.hostname)) && (
            // A bookmarked filter that matches nothing must say so —
            // silently falling back to everything would make the URL lie.
            <div class="sidebar-hint">No sessions match this filter.</div>
          )}
          {mode === 'activity' ? (

            <ActivityList
              selId={selId}
              resumingId={resumingId}
              onCloseSession={onCloseSession}
              onClick={onClose}
            />
          ) : foldersVal
            // A narrowed tab hides folders outside its scope entirely
            // (an empty header would be noise, not context) — but keeps
            // in-scope folders even when empty, so a pinned project tab
            // retains its launch target. Without a filter, all folders
            // render as before.
            .filter(f => f.sessions.length > 0
              || folderMatchesFilter(f, selectors, health.value?.hostname))
            .map(f => (
            <FolderGroup
              key={f.key}
              folder={f}
              selId={selId}
              resumingId={resumingId}
              am={am}
              peerStatus={peerStatus}
              aliveOnly={aliveOnlyVal}
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
