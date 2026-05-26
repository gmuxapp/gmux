// "Add project" modal.
//
// Three addition flows:
//   1. Peer references (subscribe to a project owned by another host)
//   2. Discovered (claim a repo on disk that isn't a project yet)
//   3. Manual local path
//
// Display, reorder, and removal of *configured* projects all live on
// the home dashboard now: the modal is intentionally additions-only,
// so the user has one place to discover-and-add and another to
// arrange-and-curate. The exported component name keeps the older
// `ManageProjectsModal` identifier for now; callers update later.

import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import {
  projects, discovered, peerProjects, peerStatusByName,
  addProject, addPeerReference,
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

// ── ManageProjectsModal ──

export function ManageProjectsModal({
  open,
  onClose,
}: {
  open: boolean
  onClose: () => void
}) {
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
      <div class="modal-panel manage-projects-modal">
        <div class="modal-header">
          <div class="modal-title">Add project</div>
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
      </div>
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
