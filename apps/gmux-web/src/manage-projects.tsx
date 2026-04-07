import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { projects, discovered, removeProject, addProject, updateProjects } from './store'
import type { ProjectItem, DiscoveredProject } from './types'

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
  const [showAllDiscovered, setShowAllDiscovered] = useState(false)
  const [manualPath, setManualPath] = useState('')
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

  // Close on backdrop click
  const handleBackdropClick = useCallback((e: MouseEvent) => {
    if (e.target === backdropRef.current) onClose()
  }, [onClose])

  const configured = projects.value
  const discoveredVal = discovered.value

  // Split discovered: active first, then the rest.
  const activeDiscovered = discoveredVal.filter(d => d.active_count > 0)
  const inactiveDiscovered = discoveredVal.filter(d => d.active_count === 0)

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

  const handleAdd = useCallback((d: DiscoveredProject) => {
    addProject({ remote: d.remote, paths: d.paths })
  }, [])

  // ── Manual add ──

  const handleManualAdd = useCallback(() => {
    const path = manualPath.trim()
    if (!path) return
    if (!path.startsWith('/') && !path.startsWith('~/')) {
      setManualError('Path must be absolute (start with / or ~/)')
      return
    }
    setManualError('')
    addProject({ paths: [path] })
    setManualPath('')
  }, [manualPath])

  const handleManualKeyDown = useCallback((e: KeyboardEvent) => {
    if (e.key === 'Enter') handleManualAdd()
  }, [handleManualAdd])

  if (!open) return null

  // Compute the visual order during drag for CSS.
  const dragItems = drag ? reorder(configured, drag.from, drag.over) : configured

  return (
    <div class="modal-backdrop" ref={backdropRef} onClick={handleBackdropClick}>
      <div class="modal-panel manage-projects-modal">
        <div class="modal-header">
          <div class="modal-title">Manage projects</div>
          <button class="modal-close" onClick={onClose}>&times;</button>
        </div>

        <div class="modal-body">
          {/* ── Configured projects ── */}
          {configured.length > 0 && (
            <section class="mp-section">
              <div class="mp-section-label">Your projects</div>
              <div class="mp-project-list">
                {dragItems.map((project, i) => (
                  <ProjectRow
                    key={project.slug}
                    project={project}
                    index={i}
                    dragging={drag !== null && project.slug === configured[drag.from]?.slug}
                    dropTarget={drag !== null && drag.over === i && drag.from !== i}
                    onDragStart={handleDragStart}
                    onDragOver={handleDragOver}
                    onDragEnd={handleDragEnd}
                    onRemove={handleRemove}
                  />
                ))}
              </div>
            </section>
          )}

          {/* ── Discovered groups ── */}
          {discoveredVal.length > 0 && (
            <section class="mp-section">
              <div class="mp-section-label">
                Discovered
                {activeDiscovered.length > 0 && (
                  <span class="mp-section-count">{activeDiscovered.length} active</span>
                )}
              </div>
              <div class="mp-discovered-list">
                {activeDiscovered.map(d => (
                  <DiscoveredRow key={d.suggested_slug} project={d} onAdd={handleAdd} />
                ))}
                {inactiveDiscovered.length > 0 && (
                  <>
                    {!showAllDiscovered ? (
                      <button
                        class="mp-show-more"
                        onClick={() => setShowAllDiscovered(true)}
                      >
                        Show {inactiveDiscovered.length} more
                      </button>
                    ) : (
                      <>
                        {inactiveDiscovered.map(d => (
                          <DiscoveredRow key={d.suggested_slug} project={d} onAdd={handleAdd} />
                        ))}
                        <button
                          class="mp-show-more"
                          onClick={() => setShowAllDiscovered(false)}
                        >
                          Show fewer
                        </button>
                      </>
                    )}
                  </>
                )}
              </div>
            </section>
          )}

          {/* ── Manual add ── */}
          <section class="mp-section">
            <div class="mp-section-label">Add by path</div>
            <div class="mp-manual-row">
              <input
                class="mp-manual-input"
                type="text"
                placeholder="/home/user/dev/my-project"
                value={manualPath}
                onInput={(e) => { setManualPath((e.target as HTMLInputElement).value); setManualError('') }}
                onKeyDown={handleManualKeyDown}
              />
              <button class="mp-manual-btn" onClick={handleManualAdd} disabled={!manualPath.trim()}>
                Add
              </button>
            </div>
            {manualError && <div class="mp-manual-error">{manualError}</div>}
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
  onRemove: (slug: string) => void
}) {
  const detail = project.remote || project.paths[0] || ''
  const shortDetail = shortenPath(detail)

  return (
    <div
      class={`mp-project-row${dragging ? ' mp-dragging' : ''}${dropTarget ? ' mp-drop-target' : ''}`}
      draggable
      onDragStart={(e) => {
        e.dataTransfer!.effectAllowed = 'move'
        // Needed for Firefox to allow drag
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
      <span class="mp-drag-handle" title="Drag to reorder">&#x2807;</span>
      <div class="mp-project-info">
        <span class="mp-project-name">{project.slug}</span>
        <span class="mp-project-detail" title={detail}>{shortDetail}</span>
      </div>
      <button
        class="mp-remove-btn"
        onClick={() => onRemove(project.slug)}
        title="Remove project"
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
