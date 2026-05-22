import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import {
  projects, discovered, peerProjects, peerStatusByName,
  removeProject, addProject, updateProjects,
  addPeerReference, removePeerReference,
} from './store'
import { PeerLabel } from './peer-label'
import type { ProjectItem, DiscoveredProject, MatchRule } from './types'

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

// ── Drag-to-reorder ──

/** State tracked during a drag operation. */
interface DragState {
  /** Index of the item being dragged. */
  from: number
  /** Current insertion target (visual feedback). */
  over: number
}

// ── ManageProjectsModal ──

export function ManageProjectsModal({
  open,
  onClose,
}: {
  open: boolean
  onClose: () => void
}) {
  const [filter, setFilter] = useState('')
  const [manualError, setManualError] = useState('')
  const [drag, setDrag] = useState<DragState | null>(null)
  const backdropRef = useRef<HTMLDivElement>(null)

  // Close on Escape
  useEffect(() => {
    if (!open) return
    const handler = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [open, onClose])

  // Reset filter when opening
  useEffect(() => {
    if (open) { setFilter(''); setManualError('') }
  }, [open])

  // Close on backdrop click
  const handleBackdropClick = useCallback((e: MouseEvent) => {
    if (e.target === backdropRef.current) onClose()
  }, [onClose])

  const configured = projects.value
  const discoveredVal = discovered.value

  // Filter discovered by the search term.
  const lowerFilter = filter.toLowerCase().trim()
  const filteredDiscovered = useMemo(() => {
    if (!lowerFilter) return discoveredVal
    return discoveredVal.filter(d =>
      d.suggested_slug.toLowerCase().includes(lowerFilter)
      || d.paths.some(p => p.toLowerCase().includes(lowerFilter))
      || (d.remote && d.remote.toLowerCase().includes(lowerFilter)),
    )
  }, [discoveredVal, lowerFilter])

  // Split filtered discovered: active first, then inactive.
  const activeDiscovered = filteredDiscovered.filter(d => d.active_count > 0)
  const inactiveDiscovered = filteredDiscovered.filter(d => d.active_count === 0)

  // Detect if filter looks like a path (for the add-by-path affordance).
  const filterIsPath = filter.trim().startsWith('/') || filter.trim().startsWith('~/')

  // ── Reorder handlers ──

  const handleDragStart = useCallback((idx: number) => {
    setDrag({ from: idx, over: idx })
  }, [])

  const handleDragOver = useCallback((idx: number) => {
    setDrag(prev => prev ? { ...prev, over: idx } : null)
  }, [])

  const handleDragEnd = useCallback(() => {
    if (!drag || drag.from === drag.over) {
      setDrag(null)
      return
    }
    const items = [...configured]
    const [moved] = items.splice(drag.from, 1)
    items.splice(drag.over, 0, moved)
    updateProjects(items)
    setDrag(null)
  }, [drag, configured])

  // ── Remove handler ──

  const handleRemove = useCallback((slug: string) => {
    removeProject(slug)
  }, [])

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

  const handleManualAdd = useCallback(() => {
    const path = filter.trim()
    if (!path) return
    if (!path.startsWith('/') && !path.startsWith('~/')) {
      setManualError('Path must be absolute (start with / or ~/)')
      return
    }
    setManualError('')
    addProject({ paths: [path] })
    setFilter('')
  }, [filter])

  const handleFilterKeyDown = useCallback((e: KeyboardEvent) => {
    if (e.key === 'Enter' && filterIsPath) handleManualAdd()
  }, [filterIsPath, handleManualAdd])

  if (!open) return null

  // Compute the visual order during drag for CSS.
  const dragItems = drag ? reorder(configured, drag.from, drag.over) : configured

  return (
    <div class="modal-backdrop" ref={backdropRef} onClick={handleBackdropClick}>
      <div class="modal-panel manage-projects-modal">
        <div class="modal-header">
          <div class="modal-title">Manage projects</div>
          <div class="modal-header-actions">
            <a
              class="mp-docs-link"
              href="https://gmux.app/reference/projects-json/#match-rules"
              target="_blank"
              rel="noopener"
              title="How match rules work"
            >?</a>
            <button class="modal-close" onClick={onClose}>&times;</button>
          </div>
        </div>

        <div class="modal-body">
          {/* ── Configured projects (owned + references) ── */}
          <section class="mp-section">
            <div class="mp-section-label">Your sidebar</div>
            {configured.length > 0 ? (
              <div class="mp-project-list">
                {dragItems.map((project, i) => (
                  <ProjectRow
                    key={`${project.peer ?? ''}::${project.slug}`}
                    project={project}
                    index={i}
                    dragging={drag !== null && itemKey(project) === itemKey(configured[drag.from])}
                    dropTarget={drag !== null && drag.over === i && drag.from !== i}
                    onDragStart={handleDragStart}
                    onDragOver={handleDragOver}
                    onDragEnd={handleDragEnd}
                    onRemove={(p) => {
                      if (p.peer) removePeerReference(p.peer, p.slug)
                      else handleRemove(p.slug)
                    }}
                  />
                ))}
              </div>
            ) : (
              <div class="mp-empty-hint">
                No projects yet. Add one from the list below, or type a path.
              </div>
            )}
          </section>

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
                type="text"
                placeholder="Filter or enter a path to add..."
                value={filter}
                onInput={(e) => { setFilter((e.target as HTMLInputElement).value); setManualError('') }}
                onKeyDown={handleFilterKeyDown}
              />
              {filterIsPath && (
                <button class="mp-manual-btn" onClick={handleManualAdd}>
                  Add
                </button>
              )}
            </div>
            {manualError && <div class="mp-manual-error">{manualError}</div>}

            <div class="mp-discovered-scroll">
              {activeDiscovered.length > 0 && activeDiscovered.map(d => (
                <DiscoveredRow key={d.suggested_slug} project={d} onAdd={handleAdd} />
              ))}
              {inactiveDiscovered.length > 0 && inactiveDiscovered.map(d => (
                <DiscoveredRow key={d.suggested_slug} project={d} onAdd={handleAdd} />
              ))}
              {filteredDiscovered.length === 0 && lowerFilter && !filterIsPath && (
                <div class="mp-empty-hint">
                  No matches. Try a different search, or enter a path to add manually.
                </div>
              )}
              {filteredDiscovered.length === 0 && !lowerFilter && (
                <div class="mp-empty-hint">
                  No unmatched sessions. Launch some sessions and they'll appear here
                  if they don't match a project.
                </div>
              )}
            </div>
          </section>
        </div>
      </div>
    </div>
  )
}

// ── Sub-components ──

function ProjectRow({
  project,
  index,
  dragging,
  dropTarget,
  onDragStart,
  onDragOver,
  onDragEnd,
  onRemove,
}: {
  project: ProjectItem
  index: number
  dragging: boolean
  dropTarget: boolean
  onDragStart: (i: number) => void
  onDragOver: (i: number) => void
  onDragEnd: () => void
  onRemove: (project: ProjectItem) => void
}) {
  const rules = project.match ?? []
  const isReference = !!project.peer

  return (
    <div
      class={`mp-project-row${dragging ? ' mp-dragging' : ''}${dropTarget ? ' mp-drop-target' : ''}`}
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
      onDrop={(e) => {
        e.preventDefault()
        onDragEnd()
      }}
      onDragEnd={onDragEnd}
    >
      <span class="mp-drag-handle" title="Drag to reorder">&#x283F;</span>
      {project.peer && <PeerLabel name={project.peer} />}
      <div class="mp-project-info">
        <span class="mp-project-name">{project.slug}</span>
        <div class="mp-project-rules">
          {isReference ? (
            <span class="mp-rule mp-rule-qualifier">reference</span>
          ) : rules.map((rule, i) => {
            const { prefix, label, qualifier } = describeRule(rule)
            const title = [prefix, label, qualifier].filter(Boolean).join(' ')
            return (
              <span key={i} class="mp-rule" title={title}>
                {prefix && <span class="mp-rule-qualifier">{prefix} </span>}
                <span class="mp-rule-label">{label}</span>
                {qualifier && <span class="mp-rule-qualifier"> {qualifier}</span>}
              </span>
            )
          })}
        </div>
      </div>
      <button
        class="mp-remove-btn"
        onClick={() => onRemove(project)}
        title={isReference ? 'Remove reference' : 'Remove project'}
      >
        &times;
      </button>
    </div>
  )
}

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

function itemKey(p: ProjectItem | undefined): string {
  if (!p) return ''
  return `${p.peer ?? ''}::${p.slug}`
}

// ── Helpers ──

function shortenPath(p: string): string {
  return p.replace(/^\/home\/[^/]+/, '~')
}

/** Reorder an array by moving item at `from` to position `to`. */
function reorder<T>(arr: T[], from: number, to: number): T[] {
  const result = [...arr]
  const [moved] = result.splice(from, 1)
  result.splice(to, 0, moved)
  return result
}
