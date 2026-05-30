// Settings modal.
//
// Deep-linkable via the `?settings` query param (see main.tsx): it
// layers over the current view without changing the path-derived
// `view`, so the background — including a live session — stays mounted.
//
// For now the modal hosts only project configuration via three
// addition flows:
//   1. Peer references (subscribe to a project owned by another host)
//   2. Discovered (claim a repo on disk that isn't a project yet)
//   3. Manual local path
//
// A tab bar (Projects / Hosts) and the configured-project manage-list
// land in later slices; today the modal is additions-only and
// reorder/removal still live on the home dashboard.

import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import {
  projects, discovered, peerProjects, peerStatusByName,
  addProject, addPeerReference, folders, updateProjects,
  removeProject, removePeerReference, localHostLabel,
  health, peers, sessions,
} from './store'
import { PeerLabel } from './peer-label'
import { HostSuffix } from './host-suffix'
import type { ProjectItem, DiscoveredProject, MatchRule, Folder } from './types'

type SettingsTab = 'projects' | 'hosts'

// ── Rule description ──

/** Human-readable parts of a single match rule. */
interface RuleDescription {
  prefix?: string   // e.g. "Remote"
  label: string     // monospace part: path or URL
  qualifier: string // dimmed suffix
}

function describeRule(rule: MatchRule): RuleDescription {
  if (rule.path) {
    const suffix = rule.exact ? ' only' : ''
    return {
      label: `${rule.path}${suffix}`,
      qualifier: '',
    }
  }

  if (rule.remote) {
    return {
      prefix: 'Remote',
      label: rule.remote,
      qualifier: 'in any directory',
    }
  }

  return { label: '(empty rule)', qualifier: '' }
}

// ── SettingsModal ──

export function SettingsModal({
  open,
  tab,
  onClose,
  onSelectTab,
}: {
  open: boolean
  tab: string
  onClose: () => void
  onSelectTab: (tab: SettingsTab) => void
}) {
  // Normalize the raw `?settings` value: anything that isn't 'hosts'
  // falls back to the projects tab (covers bare `?settings`).
  const activeTab: SettingsTab = tab === 'hosts' ? 'hosts' : 'projects'
  const [discoveredQuery, setDiscoveredQuery] = useState('')
  const [pathDraft, setPathDraft] = useState('')
  const [pathError, setPathError] = useState('')
  const backdropRef = useRef<HTMLDivElement>(null)

  // Close on Escape
  useEffect(() => {
    if (!open) return
    const handler = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [open, onClose])

  // Reset form state when opening
  useEffect(() => {
    if (open) {
      setDiscoveredQuery('')
      setPathDraft('')
      setPathError('')
    }
  }, [open])

  // Close on backdrop click
  const handleBackdropClick = useCallback((e: MouseEvent) => {
    if (e.target === backdropRef.current) onClose()
  }, [onClose])

  const configured = projects.value
  const discoveredVal = discovered.value

  // Filter discovered by the search term.
  const lowerDiscoveredQuery = discoveredQuery.toLowerCase().trim()
  const filteredDiscovered = useMemo(() => {
    if (!lowerDiscoveredQuery) return discoveredVal
    return discoveredVal.filter(d =>
      d.suggested_slug.toLowerCase().includes(lowerDiscoveredQuery)
      || d.paths.some(p => p.toLowerCase().includes(lowerDiscoveredQuery))
      || (d.remote && d.remote.toLowerCase().includes(lowerDiscoveredQuery)),
    )
  }, [discoveredVal, lowerDiscoveredQuery])

  // Split filtered discovered: active first, then inactive.
  const activeDiscovered = filteredDiscovered.filter(d => d.active_count > 0)
  const inactiveDiscovered = filteredDiscovered.filter(d => d.active_count === 0)

  // ── Add from discovered ──

  const handleAdd = useCallback(async (d: DiscoveredProject) => {
    if (d.peer) {
      // Remote suggestion: create the project on the peer (proxied),
      // then auto-add a local reference so it appears in the sidebar.
      // Gate the reference on a successful create; otherwise the user
      // gets a dangling reference (peer refused the add but viewer's
      // projects.json gains a reference to a slug that doesn't exist
      // upstream).
      //
      // Use the slug the peer actually assigned, not d.suggested_slug:
      // the peer's UniqueSlug may have deduplicated on collision
      // ("api" → "api-2"), in which case referencing the client-side
      // guess would produce an immediate dangling reference.
      try {
        const created = await addProject({ remote: d.remote, paths: d.paths }, d.peer)
        await addPeerReference(d.peer, created.slug)
      } catch {
        // addProject already logs the failure; nothing more to do here.
        // Surface in the UI eventually via a toast; out of scope for now.
      }
    } else {
      await addProject({ remote: d.remote, paths: d.paths }).catch(() => {})
    }
  }, [])

  // ── Manual add by path ──

  const handleManualAdd = useCallback(async () => {
    const path = pathDraft.trim()
    if (!path) {
      setPathError('Enter an absolute local path.')
      return
    }
    if (!path.startsWith('/') && !path.startsWith('~/')) {
      setPathError('Path must be absolute, starting with / or ~/.')
      return
    }

    setPathError('')
    try {
      await addProject({ paths: [path] })
      setPathDraft('')
    } catch {
      setPathError('Could not add that local path.')
    }
  }, [pathDraft])

  const handlePathKeyDown = useCallback((e: KeyboardEvent) => {
    if (e.key === 'Enter') handleManualAdd()
  }, [handleManualAdd])

  if (!open) return null

  return (
    <div class="modal-backdrop" ref={backdropRef} onClick={handleBackdropClick}>
      <div class="modal-panel settings-modal">
        <button class="modal-close settings-close" onClick={onClose}>&times;</button>
        <nav class="settings-rail" role="tablist">
          <div class="settings-rail-title">Settings</div>
          <button
            class={`settings-tab${activeTab === 'projects' ? ' active' : ''}`}
            role="tab"
            aria-selected={activeTab === 'projects'}
            onClick={() => onSelectTab('projects')}
          >Projects</button>
          <button
            class={`settings-tab${activeTab === 'hosts' ? ' active' : ''}`}
            role="tab"
            aria-selected={activeTab === 'hosts'}
            onClick={() => onSelectTab('hosts')}
          >Hosts</button>
        </nav>

        <div class="settings-main">
          <div class="settings-main-header">
            <span class="settings-main-title">{activeTab === 'hosts' ? 'Hosts' : 'Projects'}</span>
            {activeTab === 'projects' && (
              <a
                class="mp-docs-link"
                href="https://gmux.app/reference/projects-json/#match-rules"
                target="_blank"
                rel="noopener"
                title="How match rules work"
              >?</a>
            )}
          </div>

        {activeTab === 'hosts' ? (
          <div class="modal-body">
            <HostsTab />
          </div>
        ) : (
        <div class="modal-body">
          {/* ── Configured projects (manage: reorder + remove) ── */}
          <ConfiguredProjectsSection configured={configured} />

          {/* ── References to peer-owned projects ── */}
          <PeerReferencesSection configured={configured} />

          {/* ── Discovered projects ── */}
          <section class="mp-section">
            <div class="mp-section-label">
              Discovered
              {discoveredVal.length > 0 && (
                <span class="mp-section-count">
                  {discoveredVal.filter(d => d.active_count > 0).length} active
                </span>
              )}
            </div>

            <div class="mp-filter-row">
              <input
                class="mp-filter-input"
                type="search"
                placeholder="Search discovered projects..."
                value={discoveredQuery}
                onInput={(e) => { setDiscoveredQuery((e.target as HTMLInputElement).value) }}
              />
            </div>

            <div class="mp-discovered-scroll">
              {activeDiscovered.length > 0 && activeDiscovered.map(d => (
                <DiscoveredRow key={d.suggested_slug} project={d} onAdd={handleAdd} />
              ))}
              {inactiveDiscovered.length > 0 && inactiveDiscovered.map(d => (
                <DiscoveredRow key={d.suggested_slug} project={d} onAdd={handleAdd} />
              ))}
              {filteredDiscovered.length === 0 && lowerDiscoveredQuery && (
                <div class="mp-empty-hint">
                  No discovered projects match that search.
                </div>
              )}
              {filteredDiscovered.length === 0 && !lowerDiscoveredQuery && (
                <div class="mp-empty-hint">
                  No unmatched sessions. Launch sessions outside your configured projects
                  and they will appear here.
                </div>
              )}
            </div>
          </section>

          {/* ── Manual local path add ── */}
          <section class="mp-section mp-path-add-section">
            <div class="mp-section-label">Add local path</div>
            <div class="mp-path-add-row">
              <input
                class="mp-filter-input mp-path-input"
                type="text"
                placeholder="/home/me/dev/project"
                value={pathDraft}
                onInput={(e) => { setPathDraft((e.target as HTMLInputElement).value); setPathError('') }}
                onKeyDown={handlePathKeyDown}
              />
              <button class="mp-manual-btn" onClick={handleManualAdd} disabled={pathDraft.trim() === ''}>
                Add
              </button>
            </div>
            {pathError ? (
              <div class="mp-manual-error">{pathError}</div>
            ) : (
              <div class="mp-path-hint">Adds a local project by absolute path. Remote hosts are not affected.</div>
            )}
          </section>
        </div>
        )}
        </div>
      </div>
    </div>
  )
}

// ── Hosts tab (read-only roster) ──

/** Read-only roster of every host gmux knows about: the local host
 *  ("this host", synthesized from health), then the tailnet-discovered
 *  and configured peers. Reachability is already conveyed by the
 *  sidebar pill colors; this surface is the dig — version, session
 *  count, and the last error behind an unreachable host. */
function HostsTab() {
  const h = health.value
  const peersVal = peers.value
  // Local session count: sessions not stamped with a peer. Mirrors
  // PeerInfo.session_count for the synthesized self row.
  const localCount = sessions.value.filter(s => !s.peer).length

  return (
    <section class="mp-section">
      <div class="mp-section-label">Hosts</div>
      <div class="host-list">
        <HostRow
          self
          name={h?.hostname ?? 'this host'}
          status="connected"
          sessionCount={localCount}
          version={h?.version}
        />
        {peersVal.map(p => (
          <HostRow
            key={p.name}
            name={p.name}
            status={p.status}
            sessionCount={p.session_count}
            version={p.version}
            lastError={p.last_error}
            local={p.local}
          />
        ))}
      </div>
    </section>
  )
}

function HostRow({
  name, self, status, sessionCount, version, lastError, local,
}: {
  name: string
  self?: boolean
  status: string
  sessionCount: number
  version?: string
  lastError?: string
  local?: boolean
}) {
  const connected = status === 'connected'
  return (
    <div class="host-row">
      <div class="host-row-main">
        <span class={`host-status-dot${connected ? ' connected' : ''}`} aria-hidden="true" />
        {self
          ? <span class="host-self-tag">this host</span>
          : <PeerLabel name={name} />}
        <span class="host-name">
          {name}
          {local && <span class="host-local-tag">local</span>}
        </span>
        <div class="host-meta">
          <span class="host-status-label">{status}</span>
          <span class="host-sep">·</span>
          <span>{sessionCount} session{sessionCount === 1 ? '' : 's'}</span>
          {version && <><span class="host-sep">·</span><span>v{version}</span></>}
        </div>
      </div>
      {!connected && lastError && (
        <div class="host-error">{lastError}</div>
      )}
    </div>
  )
}

// ── Configured projects (manage-list) ──

/** Drag-to-reorder transient state. `from` is the index lifted off,
 *  `over` is the current insertion target. */
interface DragState { from: number; over: number }

function reorder<T>(arr: readonly T[], from: number, to: number): T[] {
  const out = [...arr]
  const [moved] = out.splice(from, 1)
  out.splice(to, 0, moved)
  return out
}

/** The unified "Your projects" list at the top of the Projects tab:
 *  every configured project (local + peer references) in sidebar order,
 *  management-only — drag-to-reorder and remove, no navigation, no
 *  launch. This is the single ordering that drives the sidebar; the
 *  list maps 1:1 to projects.json items[] (which buildProjectFolders
 *  preserves). Reference rows are distinguished by a leading colored
 *  PeerLabel chip. */
function ConfiguredProjectsSection({ configured }: { configured: ProjectItem[] }) {
  const foldersVal = folders.value
  const [drag, setDrag] = useState<DragState | null>(null)
  const dragItems = drag ? reorder(configured, drag.from, drag.over) : configured

  const handleDragStart = useCallback((i: number) => setDrag({ from: i, over: i }), [])
  const handleDragOver = useCallback((i: number) => {
    setDrag(prev => prev ? { ...prev, over: i } : null)
  }, [])
  const handleDragEnd = useCallback(() => {
    // Commit before clearing. State-setter updaters must stay pure
    // (Preact may invoke them more than once), so the side effect
    // lives outside the updater.
    if (drag && drag.from !== drag.over) {
      updateProjects(reorder(configured, drag.from, drag.over))
    }
    setDrag(null)
  }, [drag, configured])

  const handleRemove = useCallback((p: ProjectItem) => {
    if (p.peer) removePeerReference(p.peer, p.slug)
    else removeProject(p.slug)
  }, [])

  if (configured.length === 0) return null

  return (
    <section class="mp-section">
      <div class="mp-section-label">Your projects</div>
      <div class="mp-configured-list">
        {dragItems.map((p, i) => {
          const folderKey = `${p.peer ?? ''}::${p.slug}`
          const folder = foldersVal.find(f => f.key === folderKey)
          if (!folder) return null
          return (
            <ConfiguredProjectRow
              key={folderKey}
              folder={folder}
              project={p}
              index={i}
              dragging={drag !== null && drag.from === configured.indexOf(p)}
              dropTarget={drag !== null && drag.over === i && drag.from !== i}
              onDragStart={handleDragStart}
              onDragOver={handleDragOver}
              onDragEnd={handleDragEnd}
              onRemove={handleRemove}
            />
          )
        })}
      </div>
    </section>
  )
}

function ConfiguredProjectRow({
  folder: f, project, index,
  dragging, dropTarget,
  onDragStart, onDragOver, onDragEnd, onRemove,
}: {
  folder: Folder
  project: ProjectItem
  index: number
  dragging: boolean
  dropTarget: boolean
  onDragStart: (i: number) => void
  onDragOver: (i: number) => void
  onDragEnd: () => void
  onRemove: (project: ProjectItem) => void
}) {
  const alive = f.sessions.filter(s => s.alive).length
  const resumable = f.sessions.filter(s => !s.alive && s.resumable).length
  const isReference = !!project.peer
  return (
    <div
      class={`mp-configured-row${dragging ? ' dragging' : ''}${dropTarget ? ' drop-target' : ''}`}
      draggable
      onDragStart={(e) => {
        e.dataTransfer!.effectAllowed = 'move'
        e.dataTransfer!.setData('text/plain', '')
        onDragStart(index)
      }}
      onDragOver={(e) => {
        e.preventDefault()
        e.dataTransfer!.dropEffect = 'move'
        onDragOver(index)
      }}
      onDrop={(e) => { e.preventDefault() }}
      onDragEnd={onDragEnd}
    >
      <span class="mp-configured-drag" title="Drag to reorder" aria-hidden="true">⠿</span>
      {isReference && <PeerLabel name={project.peer!} />}
      <div class="mp-configured-info">
        <span class="mp-configured-name">
          {f.name}
          <HostSuffix peer={f.peer ?? localHostLabel.value} local={!f.peer} />
        </span>
        <span class="mp-configured-count">
          {alive > 0 && <span class="mp-configured-alive">{alive} alive</span>}
          {alive > 0 && resumable > 0 && <span class="mp-configured-rest"> · </span>}
          {resumable > 0 && <span class="mp-configured-rest">{resumable} resumable</span>}
          {alive === 0 && resumable === 0 && <span class="mp-configured-rest">no sessions</span>}
        </span>
      </div>
      <button
        class="mp-configured-remove"
        onClick={(e) => { e.preventDefault(); e.stopPropagation(); onRemove(project) }}
        title={isReference ? 'Remove reference' : 'Remove project'}
      >
        ×
      </button>
    </div>
  )
}

// ── Sub-components ──

function DiscoveredRow({
  project,
  onAdd,
}: {
  project: DiscoveredProject
  onAdd: (d: DiscoveredProject) => void
}) {
  const detail = project.remote || project.paths[0] || ''
  const shortDetail = shortenPath(detail)

  return (
    <div class="mp-discovered-row" onClick={() => onAdd(project)}>
      {project.peer && <PeerLabel name={project.peer} />}
      <div class="mp-project-info">
        <span class="mp-project-name">
          {project.suggested_slug}
          {project.active_count > 0 && (
            <span class="mp-active-badge">{project.active_count}</span>
          )}
        </span>
        <span class="mp-project-detail" title={detail}>{shortDetail}</span>
      </div>
      <span class="mp-add-label">+ Add</span>
    </div>
  )
}

/** Lists peer-owned projects that the viewer hasn't referenced yet,
 *  one section per connected peer. Each row adds a reference via
 *  addPeerReference; the new folder appears in the sidebar on the
 *  next render. */
function PeerReferencesSection({ configured }: { configured: ProjectItem[] }) {
  const peerProjectsByPeer = peerProjects.value
  const statuses = peerStatusByName.value
  const referenced = useMemo(() => {
    const set = new Set<string>()
    for (const p of configured) {
      if (p.peer) set.add(`${p.peer}::${p.slug}`)
    }
    return set
  }, [configured])

  const entries: { peer: string; slug: string }[] = []
  for (const peer of Object.keys(peerProjectsByPeer).sort()) {
    if (statuses.get(peer) !== 'connected') continue
    for (const sp of peerProjectsByPeer[peer]) {
      if (referenced.has(`${peer}::${sp.slug}`)) continue
      entries.push({ peer, slug: sp.slug })
    }
  }

  if (entries.length === 0) return null
  return (
    <section class="mp-section">
      <div class="mp-section-label">From other hosts</div>
      <div class="mp-project-list">
        {entries.map(({ peer, slug }) => (
          <div
            key={`${peer}::${slug}`}
            class="mp-discovered-row"
            onClick={() => addPeerReference(peer, slug)}
          >
            <PeerLabel name={peer} />
            <div class="mp-project-info">
              <span class="mp-project-name">{slug}</span>
            </div>
            <span class="mp-add-label">+ Add</span>
          </div>
        ))}
      </div>
    </section>
  )
}

// ── Helpers ──

function shortenPath(p: string): string {
  return p.replace(/^\/home\/[^/]+/, '~')
}
