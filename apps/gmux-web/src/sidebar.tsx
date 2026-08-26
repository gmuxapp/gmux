/**
 * Sidebar: project folders, session items, and the navigation shell.
 *
 * Reads shared state directly from the store (signals). Only action
 * callbacks and the mobile open/close toggle are passed as props.
 */

import { useState, useCallback, useRef, useEffect } from 'preact/hooks'
import { needsReveal } from './sidebar-reveal'
import { hasSessionSlugCollision, sessionPath, viewToPath } from './routing'
import { FamilyIcon } from './family-icon'
import { familyDrawerRoot } from './family-drawer-state'
import { selectorLabel, folderMatchesFilter, type Selector } from './tab-filter'
import { reorderKeysForFolder } from './projects'
import { LaunchButton } from './launcher'
import { useArrivalPulse } from './use-arrival-pulse'
import {
  folders, familySelectedId, sessions,
  activityMap, projects, connState, health, peers,
  collapsedFolders, toggleFolderCollapsed,
  updateProjects, reorderSessions,
  peerStatusByName, isSessionUnavailable, localPeerNames, ownDotState, selectedId,
  familyActivityById, familySlotById, type FamilySlot,
  unreadCount, localHostLabel, unresolvedHosts, duplicateConversationFiles,
  sidebarActivity, sidebarMode, setSidebarMode,
  activeSelectors, removeSelector, setHostFilter,
  aliveOnly, setAliveOnly, tabHref, navigate, sessionStreamWarnings, sessionStreamOmittedTotal,
  type DotState,
} from './store'
import { HostSuffix } from './host-suffix'
import { SessionRow } from './session-row'
import {
  childTrailTitle, familyActivityLabel, familyMemberGlyph, familySegments,
  type FamilyActivity,
} from './family'
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

/** Deep link to any session by id (the child rows can't reuse the
 *  folder's slug: a descendant may live in another project). */
function sessionHref(session: Session): string | undefined {
  const path = viewToPath({ kind: 'session', sessionId: session.id }, projects.value, sessions.value)
  return path ? tabHref(path) : undefined
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
  // The row's own status, muted for selection ("nothing is unread if
  // you're already looking at it") by `ownDotState` — a root row shows
  // the root's state, never a roll-up of its children's, so a working
  // child cannot masquerade as a working root.
  const dotState = resuming ? 'working' : rawDotState
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
      onClick={() => { onClick?.() }}
      onAuxClick={(e) => { if (e.button === 1 && onClose) { e.preventDefault(); onClose() } }}
      onDragStart={(e) => {
        e.dataTransfer!.effectAllowed = 'move'
        e.dataTransfer!.setData('text/plain', '')
        onDragStart?.()
      }}
      onDragOver={(e) => { e.preventDefault(); e.dataTransfer!.dropEffect = 'move'; onDragOver?.() }}
      // Family entries wrap this row in a group that is itself a drop
      // target; without this the drop runs both handlers in one dispatch,
      // before a re-render can clear `drag`, and the reorder is sent twice.
      onDrop={(e) => { e.preventDefault(); e.stopPropagation(); onDragEnd?.() }}
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

/** The family button: the static opener at the head of the subordinate
 *  row, carrying the activity segments while nothing in the family is
 *  selected. The icon never moves — it is the row's fixed identity —
 *  only what follows it changes: counts inside the button, or the
 *  member row beside it. */
function FamilyOpenButton({
  activity,
  rootId,
  rootHref,
  onClick,
}: {
  activity: FamilyActivity | undefined
  rootId: string
  rootHref: string
  onClick?: () => void
}) {
  const segments = activity ? familySegments(activity) : []
  const label = activity ? familyActivityLabel(activity) : 'Session family'
  return (
    <button
      type="button"
      class="family-activity"
      title={label}
      aria-label={`${label}. Open family panel`}
      aria-controls="agent-family-drawer"
      onClick={() => {
        // A second press dismisses: if the panel is already open and
        // there is nowhere left to navigate, the press can only mean
        // "put it away". While elsewhere in the family it navigates to
        // the root instead, and the open drawer simply follows.
        if (familyDrawerRoot.value === rootId && selectedId.value === rootId) {
          familyDrawerRoot.value = null
          return
        }
        // Name the family first, then go: the header opens the drawer
        // when it arrives somewhere that family owns. Stating the want
        // before the trip also keeps it true if navigation ever became
        // synchronous — the arrival would already agree with it.
        familyDrawerRoot.value = rootId
        navigate(rootHref)
        onClick?.()
      }}
    >
      {/* The family mark, same as the header's trigger: this line is
        * the standard family numbers — everyone beneath the root — not
        * "the others", so it anchors to the root's glyph column rather
        * than indenting under whichever member row happens to be shown. */}
      {/* The mark is the whole visible button when a member row sits
        * beside it — it's the same icon as the header's trigger, so it
        * already means "family panel" without a suffix. */}
      <span class="family-glyph" aria-hidden="true"><FamilyIcon class="family-activity-icon" /></span>
      {/* Glyphs are decoration; the sentence carries the meaning. */}
      {segments.length > 0 && (
        <span class="family-activity-glyphs" aria-hidden="true">
          {segments.map(segment => (
            <span key={segment.state} class="family-activity-seg">
              {segment.dot
                ? <span class={`family-glyph session-dot-indicator ${segment.dot}`} />
                : <span class="family-glyph family-proc">$</span>}
              {segment.count}
            </span>
          ))}
        </span>
      )}
      <span class="sr-only">{label}</span>
    </button>
  )
}

/** The sidebar's family entry: a root row and at most one subordinate
 *  row, led by the static family button. The button carries the family
 *  counts until a descendant takes selection; then the counts yield to
 *  the member's own row beside it, and return the moment selection
 *  leaves the family. No history: the member row *is* the selection.
 *
 *  Selection and hit areas nest, the way the sessions do:
 *   - the group is the root's area. Hovering anywhere in it highlights
 *     the whole group, and any click that doesn't land on the member
 *     row selects the root — including the slack around its nested
 *     targets, which has nothing better to do;
 *   - the member row and family button are their own areas inside that
 *     one, each with its own hover treatment;
 *   - the background says *which family* you're in, the accent bar says
 *     *which row*, so selecting a member keeps the group lit and moves
 *     the bar down to it.
 *
 *  One more rule holds the content together: the root row's dot is the
 *  root session's own status, so a working child never masquerades as
 *  a working root. The counts, by contrast, are a summary and may well
 *  include the member selected within — the standard family numbers are
 *  a fact about the family, the same on every surface, and subtracting
 *  whatever a surface happens to name made them wobble with unrelated
 *  state (see `familyActivityById`).
 */
function FamilyEntry({
  selected,
  rootId,
  rootHref,
  slot,
  slotHref,
  slotTrail,
  activity,
  onClick,
  onDragOver,
  onDragEnd,
  children,
}: {
  /** Something in this family is selected — root or member. */
  selected: boolean
  rootId: string
  rootHref: string
  slot?: FamilySlot
  slotHref?: string
  /** Root › … › member trail, for the member row's hover title. */
  slotTrail?: string
  activity: FamilyActivity | undefined
  onClick?: () => void
  /** Reorder drop target for the whole group, not just the root row. */
  onDragOver?: () => void
  onDragEnd?: () => void
  /** The root's own `SessionItem`. */
  children: preact.ComponentChildren
}) {
  const member = slot?.session
  // One decision, made in `family.ts` so it can be tested: which glyph
  // this member gets, and what state it carries. This row is the only
  // place this member is named, so its glyph has to be able to say
  // `unread` — the line below counts it among many, but a count is not
  // a way to see which member needs you.
  const glyph = member
    ? familyMemberGlyph(member, ownDotState(member, activityMap.value, selectedId.value))
    : null
  return (
    // Not focusable on purpose: the group's own rows are the keyboard
    // targets, and this only hands the pointer the slack between them,
    // which leads somewhere those rows already go.
    <div
      class={`session-family${selected ? ' selected' : ''}`}
      onClick={(e) => {
        // Anything with its own target keeps it (root row, member row,
        // close button); the leftovers fall through to the root.
        if ((e.target as HTMLElement | null)?.closest('a, button')) return
        // The slack is a convenience, not a link, so it declines every
        // gesture a link would answer differently: a modified click
        // wants a new tab or a download, and a click that ends a text
        // selection wants the text, not a navigation. Doing otherwise
        // makes the entry feel like it grabs at the pointer.
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return
        if (!(getSelection()?.isCollapsed ?? true)) return
        e.preventDefault()
        navigate(rootHref)
        onClick?.()
      }}
      // A family entry is one reorder target. Without this the drop is
      // only accepted over the root row, so the taller the entry grows
      // the more of it silently refuses the drag.
      onDragOver={onDragOver && ((e) => { e.preventDefault(); e.dataTransfer!.dropEffect = 'move'; onDragOver() })}
      onDrop={onDragEnd && ((e) => { e.preventDefault(); onDragEnd() })}
    >
      {children}
      {(member || activity) && (
        <div class="family-sub">
          {/* The static head of the row: same button in both states, so
            * the panel's entry point never teleports. Only the content
            * after it changes — counts inside the button, or the member
            * row beside it. */}
          <FamilyOpenButton
            activity={member ? undefined : activity}
            rootId={rootId}
            rootHref={rootHref}
            onClick={onClick}
          />
          {member && (
            <a
              class="family-slot selected"
              href={slotHref}
              aria-current="page"
              title={slotTrail}
              onClick={() => onClick?.()}
            >
              {/* One fixed-width glyph column: the member's status, or
                * its `$` if it's a process. A quiet member shows nothing —
                * the family button beside the row already says it hangs
                * off the root, and a filler arrow next to that icon read
                * as a second, mysterious control. */}
              {glyph?.kind === 'process'
                ? <span class={`family-glyph family-proc${glyph.running ? '' : ' done'}`} aria-hidden="true">$</span>
                : glyph?.kind === 'dot'
                ? <span class={`family-glyph session-dot-indicator ${glyph.state}`} aria-hidden="true" />
                : null}
              <span class="family-slot-title">{member.title}</span>
            </a>
          )}
        </div>
      )}
    </div>
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
  // Snapshot-wide family derivations, read once per folder render. All
  // three are O(n) maps built once per session-list identity in the
  // store, so a folder's rows stay O(rows) lookups.
  const activityById = familyActivityById.value
  const slots = familySlotById.value
  const rawSelId = selectedId.value

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

  const headerRef = useRef<HTMLDivElement>(null)
  // Collapsing removes the rows below the header. Because headers are
  // sticky, if you're scrolled down inside this folder its box can end
  // up entirely above the viewport once its rows vanish — the header you
  // just clicked disappears upward, which is disorienting. So when (and
  // only when) collapsing would push the clicked header off the top,
  // pull the scroll position back so it stays where it was.
  const handleToggleCollapse = () => {
    const el = headerRef.current
    const scroll = el?.closest('.sidebar-scroll') as HTMLElement | null
    const wasCollapsed = collapsed
    const beforeTop = el && scroll
      ? el.getBoundingClientRect().top - scroll.getBoundingClientRect().top
      : 0
    toggleFolderCollapsed(folder.key)
    if (wasCollapsed || !el || !scroll) return // only correct when collapsing
    requestAnimationFrame(() => {
      const afterTop = el.getBoundingClientRect().top - scroll.getBoundingClientRect().top
      if (afterTop < 0) scroll.scrollTop += afterTop - beforeTop
    })
  }
  return (
    <div class="folder">
      <div class="folder-header" ref={headerRef}>
        <button
          type="button"
          class={`folder-name${folder.missing ? ' missing' : ''}${folder.unresolved ? ' unresolved' : ''}`}
          aria-expanded={!collapsed}
          title={folder.unresolved
            ? `Host “${folder.peer}” isn't a connected or manually-added host — it may have been renamed or removed. Open Settings → Hosts to remap or remove it.`
            : folder.missing
            ? `${folder.name} no longer exists on ${folder.peer} — remove this reference in Settings → Projects.`
            : collapsed ? `Expand ${folder.name}` : `Collapse ${folder.name}`}
          onClick={handleToggleCollapse}
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
        {shown.map((s, i) => {
          const href = tabHref(sessionPath(folder.slug, s, folder.peer, hasSessionSlugCollision(s, sessions.value, projects.value)))
          const activity = activityById.get(s.id)
          const slot = slots.get(s.id)
          const item = (
            <SessionItem
              key={s.id}
              session={s}
              href={href}
              // `selId` maps a selected descendant onto its root row.
              // The entry now draws that selection on the member's own
              // row instead, so the root row only lights up for itself.
              selected={selId === s.id && !slot}
              resuming={resumingId === s.id}
              // Root row = root's own status. The family roll-up lives on
              // the summary line below it (see FamilyEntry).
              dotState={ownDotState(s, am, rawSelId)}
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
          )
          // No member to name and nothing else to count: one plain row.
          if (!slot && !activity) return item
          return (
            <FamilyEntry
              key={s.id}
              selected={selId === s.id}
              rootId={s.id}
              rootHref={href}
              onDragOver={dragDisabled ? undefined : () => handleDragOver(i)}
              onDragEnd={dragDisabled ? undefined : () => handleDragEnd(visible)}
              slot={slot}
              slotHref={slot && sessionHref(slot.session)}
              slotTrail={slot && childTrailTitle(s, slot.ancestors, slot.session)}
              activity={activity}
              onClick={onClick}
            >
              {item}
            </FamilyEntry>
          )
        })}
      </div>
      )}
    </div>
  )
}

/** Always-visible Projects/Activity switch: under the header on desktop,
 *  above the logo/header row on touch layouts (see the coarse-pointer
 *  rules on .sidebar-view-toggle). Two equal buttons rather than one
 *  cycling control — a glance answers both where you are and what the
 *  alternative is. The active fill is the sidebar's selected-row fill:
 *  "you are here" in the language the rows below already speak. */
function ViewToggle({ mode }: { mode: 'projects' | 'activity' }) {
  return (
    <div class="sidebar-view-toggle" role="group" aria-label="Sidebar view">
      <button
        class={`sidebar-view-btn${mode === 'projects' ? ' active' : ''}`}
        aria-pressed={mode === 'projects'}
        onClick={() => setSidebarMode('projects')}
      >Projects</button>
      <button
        class={`sidebar-view-btn${mode === 'activity' ? ' active' : ''}`}
        aria-pressed={mode === 'activity'}
        onClick={() => setSidebarMode('activity')}
      >Activity</button>
    </div>
  )
}

/** Compact popover behind the header's arrange icon. Two concerns,
 *  two lifetimes:
 *    Host  — narrows the tab to one host (`*@host` in ?filter=).
 *    Alive only — hides resumable corpses; sessionStorage (per tab).
 *  The Projects/Activity switch moved out to the always-visible
 *  ViewToggle above. One entry point, instant switching — the list is
 *  the preview. */
function ViewMenu({ open, onToggle }: { open: boolean; onToggle: () => void }) {
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

  // compact=false for today (two-line with age), true for older buckets
  // (single-line project · title — the day heading carries the time).
  const renderRow = (s: Session, compact: boolean) => {
    const folder = folderBySessionId.get(s.id)
    if (!folder) return null
    return (
      <SessionRow
        key={s.id}
        session={s}
        href={tabHref(sessionPath(folder.slug, s, folder.peer, hasSessionSlugCollision(s, sessions.value, projects.value)))}
        selected={selId === s.id}
        resuming={resumingId === s.id}
        compact={compact}
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
          {sec.sessions.map(s => renderRow(s, sec.label !== null))}
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
  const selId = familySelectedId.value
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

  // Scroll-to-top pill (Activity view only): show once you're a decent
  // way into the *content*, so the top items are one tap away without a
  // long scroll back. Measured relative to the first section rather than
  // a fixed scrollTop, because the top thumb gap is itself hundreds of px
  // on tall phones — a fixed threshold would fire while still scrolling
  // through the blank gap.
  const [scrolledDown, setScrolledDown] = useState(false)
  useEffect(() => {
    const el = scrollRef.current
    if (!el) return
    // Projects view has no scroll-to-top button, so don't track scroll
    // there (avoids a querySelector-miss on every wheel tick and stale
    // state nothing renders).
    if (mode !== 'activity') { setScrolledDown(false); return }
    // Selecting Activity jumps back to the top (the most-recent items).
    // This deliberately runs after the reveal effect above (declared
    // first, so it fires first on a mode change), overriding its
    // scroll-selected-into-view for the view switch — within Activity a
    // later selId change still reveals normally.
    el.scrollTop = 0
    const onScroll = () => {
      const first = el.querySelector('.sidebar-activity-section')
      setScrolledDown(
        first
          ? first.getBoundingClientRect().top < el.getBoundingClientRect().top - 240
          : el.scrollTop > 240,
      )
    }
    onScroll()
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => el.removeEventListener('scroll', onScroll)
  }, [mode])

  // folder.sessions is already the shown set (see store.ts
  // sidebarSessions), so this is just the visible session count.
  const totalVisible = foldersVal.reduce((n, f) => n + f.sessions.length, 0)
  const connected = connState.value === 'connected'
  const streamWarnings = sessionStreamWarnings.value
  const omittedSessionCount = sessionStreamOmittedTotal.value
  const detailedOmittedCount = streamWarnings.reduce((sum, warning) => sum + warning.count, 0)
  const suppressedOmittedCount = Math.max(0, omittedSessionCount - detailedOmittedCount)
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
        <ViewToggle mode={mode} />
        <FilterChips selectors={selectors} />
        <div class="sidebar-scroll" ref={scrollRef} onClick={() => menuOpen && setMenuOpen(false)}>
          {/* Every row lives on one opaque slab so the scrollport's own
            * background can act as a fixed backdrop behind it (see the
            * cavity in styles.css). Purely presentational. */}
          <div class="sidebar-list">
          {omittedSessionCount > 0 && (
            <div
              class="sidebar-stream-warning"
              role="status"
              title={[
                ...streamWarnings.map(w => `${w.id}: ${w.message || w.code}`),
                ...(suppressedOmittedCount > 0 ? [`${suppressedOmittedCount} additional omitted sessions`] : []),
              ].join('\n')}
            >
              ⚠ {omittedSessionCount} {omittedSessionCount === 1 ? 'session is' : 'sessions are'} omitted from the live list
            </div>
          )}
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
        </div>
        {mode === 'activity' && scrolledDown && (
          <button
            type="button"
            class="scroll-top-btn"
            aria-label="Scroll to top"
            title="Scroll to top"
            onClick={() => { if (scrollRef.current) scrollRef.current.scrollTop = 0 }}
          >
            Top ↑
          </button>
        )}
      </aside>
    </>
  )
}
