/**
 * FileTree — filesystem browser panel for the sidebar.
 *
 * Thin Preact wrapper around @pierre/trees (vanilla FileTree class).
 * The library owns its shadow DOM; this component handles:
 *   - GET /v1/fs/{slug}/walk   → paths fed to tree.resetPaths()
 *   - GET /v1/git/{slug}/files  → per-file status fed to tree.setGitStatus()
 *   - rename / move / delete / create callbacks → REST mutations + refresh
 *   - External file drop (Finder → upload)
 *   - Show-hidden toggle, new-file/new-folder buttons
 *   - Delete confirmation modal (vanilla <dialog> pattern, rendered in Preact)
 *
 * All filesystem mutations go through /v1/fs/{slug}/* REST endpoints.
 */

import { useState, useCallback, useRef, useEffect } from 'preact/hooks'
import { FileTree as PierreFileTree, type GitStatusEntry } from '@pierre/trees'
import {
  markPendingLaunch,
  navigateToMarkdownEditor,
  navigateToImageViewer,
  sessions,
  navigateToSession,
} from './store'
import type { Session } from './types'
import { GitStatus } from './git-status'

// ── Types ──────────────────────────────────────────────────────────────────

export interface FileEntry {
  name: string
  type: 'file' | 'dir'
  size?: number
  mtime?: string
}

export interface TreeNode {
  name: string
  type: 'file' | 'dir'
  /** Path relative to project root, e.g. "src/main.go" */
  path: string
}

// ── Pure helpers (exported for tests — backward compat) ─────────────────────

/** Normalize a relative path: strip leading slashes, collapse redundant separators. */
export function normalizeFsPath(rel: string): string {
  return rel.replace(/^\/+/, '').replace(/\/+/g, '/').replace(/\/$/, '')
}

/** Join two relative path segments. */
export function joinFsPath(parent: string, name: string): string {
  if (!parent) return name
  return `${parent}/${name}`
}

/** Returns true if a filename is hidden (starts with '.'). */
export function isHiddenName(name: string): boolean {
  return name.startsWith('.')
}

/** Build a sorted TreeNode[] from raw FileEntry list plus a parent path. */
export function buildTreeNodes(entries: FileEntry[], parentPath: string): TreeNode[] {
  const dirs = entries.filter(e => e.type === 'dir').sort((a, b) => a.name.localeCompare(b.name))
  const files = entries.filter(e => e.type === 'file').sort((a, b) => a.name.localeCompare(b.name))
  return [...dirs, ...files].map(e => ({
    name: e.name,
    type: e.type,
    path: joinFsPath(parentPath, e.name),
  }))
}

/**
 * Remove a deleted path and all of its children from an expanded set.
 * Returns a new Set; the original is not mutated.
 */
export function pruneExpanded(expanded: Set<string>, deletedPath: string): Set<string> {
  const next = new Set(expanded)
  const prefix = deletedPath + '/'
  for (const p of next) {
    if (p === deletedPath || p.startsWith(prefix)) next.delete(p)
  }
  return next
}

/**
 * Returns true if a DragEvent carries external files (e.g. from Finder/Explorer).
 */
export function isExternalFileDrop(dt: DataTransfer): boolean {
  return dt.files.length > 0
}

/**
 * Returns the first alive session in `cwd` whose last command argument
 * matches the leaf filename of `relPath`.
 */
export function findOpenFileSession(
  sessionList: Session[],
  cwd: string,
  relPath: string,
): Session | undefined {
  const base = relPath.split('/').pop() ?? relPath
  return sessionList.find(
    s =>
      s.alive &&
      s.cwd === cwd &&
      s.command.length > 0 &&
      s.command[s.command.length - 1] === base,
  )
}

export function copyRelativePath(path: string): Promise<void> {
  return navigator.clipboard.writeText(path)
}

// ── API helpers ─────────────────────────────────────────────────────────────

async function apiFetch(
  method: string,
  url: string,
  body?: unknown,
): Promise<{ ok: boolean; data?: unknown; error?: { code: string; message: string } }> {
  const opts: RequestInit = { method }
  if (body !== undefined) {
    opts.headers = { 'Content-Type': 'application/json' }
    opts.body = JSON.stringify(body)
  }
  const resp = await fetch(url, opts)
  return resp.json()
}

async function apiWalkPaths(slug: string): Promise<string[]> {
  const resp = await apiFetch('GET', `/v1/fs/${encodeURIComponent(slug)}/walk`)
  if (!resp.ok) throw new Error((resp.error?.message) ?? 'walk failed')
  return resp.data as string[]
}

async function apiGitFiles(slug: string): Promise<GitStatusEntry[]> {
  const resp = await apiFetch('GET', `/v1/git/${encodeURIComponent(slug)}/files`)
  if (!resp.ok) return []
  return resp.data as GitStatusEntry[]
}

async function apiMkdir(slug: string, path: string): Promise<void> {
  const resp = await apiFetch('POST', `/v1/fs/${encodeURIComponent(slug)}/mkdir`, { path })
  if (!resp.ok) throw new Error((resp.error?.message) ?? 'mkdir failed')
}

async function apiCreateFile(slug: string, path: string): Promise<void> {
  const resp = await apiFetch('POST', `/v1/fs/${encodeURIComponent(slug)}/create`, { path })
  if (!resp.ok) throw new Error((resp.error?.message) ?? 'create failed')
}

async function apiRename(slug: string, from: string, to: string): Promise<void> {
  const resp = await apiFetch('POST', `/v1/fs/${encodeURIComponent(slug)}/rename`, { from, to })
  if (!resp.ok) throw new Error((resp.error?.message) ?? 'rename failed')
}

async function apiMove(slug: string, from: string, to: string): Promise<void> {
  const resp = await apiFetch('POST', `/v1/fs/${encodeURIComponent(slug)}/move`, { from, to })
  if (!resp.ok) throw new Error((resp.error?.message) ?? 'move failed')
}

async function apiDelete(slug: string, path: string, recursive: boolean): Promise<void> {
  const resp = await apiFetch('DELETE', `/v1/fs/${encodeURIComponent(slug)}/item`, {
    path,
    recursive,
  })
  if (!resp.ok) throw new Error((resp.error?.message) ?? 'delete failed')
}

async function apiOpen(slug: string, path: string): Promise<void> {
  markPendingLaunch()
  const resp = await apiFetch('POST', `/v1/fs/${encodeURIComponent(slug)}/open`, { path })
  if (!resp.ok) throw new Error((resp.error?.message) ?? 'open failed')
}

async function apiOpenBrowser(slug: string, path: string): Promise<void> {
  const resp = await apiFetch('POST', `/v1/fs/${encodeURIComponent(slug)}/open-browser`, { path })
  if (!resp.ok) throw new Error((resp.error?.message) ?? 'open-browser failed')
}

async function apiUpload(slug: string, dir: string, files: FileList): Promise<void> {
  const form = new FormData()
  for (let i = 0; i < files.length; i++) form.append('file', files[i])
  const resp = await fetch(
    `/v1/fs/${encodeURIComponent(slug)}/upload?dir=${encodeURIComponent(dir)}`,
    { method: 'POST', body: form },
  )
  const json = await resp.json()
  if (!json.ok) throw new Error((json.error?.message) ?? 'upload failed')
}

// ── Minimal icons for the header ────────────────────────────────────────────

const svgProps = {
  viewBox: '0 0 12 12',
  width: '12',
  height: '12',
  fill: 'none',
  stroke: 'currentColor',
  'stroke-width': '1.4',
  'stroke-linecap': 'round' as const,
  'stroke-linejoin': 'round' as const,
}

const IconPlus = () => (
  <svg {...svgProps} class="ft-action-icon"><path d="M6 2v8M2 6h8"/></svg>
)
const IconEye = () => (
  <svg {...svgProps} class="ft-action-icon">
    <path d="M1 6c0 0 2-4 5-4s5 4 5 4-2 4-5 4-5-4-5-4z"/>
    <circle cx="6" cy="6" r="1.5"/>
  </svg>
)
const IconEyeOff = () => (
  <svg {...svgProps} class="ft-action-icon">
    <path d="M1 6c0 0 2-4 5-4s5 4 5 4-2 4-5 4-5-4-5-4z"/>
    <circle cx="6" cy="6" r="1.5"/>
    <path d="M2 2l8 8"/>
  </svg>
)
const IconFolderPlus = () => (
  <svg {...svgProps} class="ft-action-icon">
    <path d="M1 4h10v7H1z"/><path d="M1 4l1.5-2H5l1 1.5H1"/><path d="M6 8V6M5 7h2"/>
  </svg>
)

// ── Helpers ─────────────────────────────────────────────────────────────────

/** Strip trailing slash from directory paths (for API calls). */
function stripSlash(p: string): string {
  return p.endsWith('/') ? p.slice(0, -1) : p
}

/** Filter paths to hide hidden files/directories unless showHidden is true. */
function filterPaths(paths: string[], showHidden: boolean): string[] {
  if (showHidden) return paths
  return paths.filter(p =>
    p.split('/').every(seg => !seg || !isHiddenName(seg)),
  )
}

// ── Component ────────────────────────────────────────────────────────────────

export interface FileTreeProps {
  projectSlug: string
  /** Absolute filesystem path of the project root (used for display only). */
  cwd: string
  /** Called when the sidebar should close on mobile after a navigation. */
  onMobileClose?: () => void
}

export function FileTree({ projectSlug, cwd }: FileTreeProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const treeRef = useRef<PierreFileTree | null>(null)

  const [showHidden, setShowHidden] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [externalDragOver, setExternalDragOver] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<{
    path: string
    name: string
    isFolder: boolean
  } | null>(null)

  // Refs so that callbacks captured at mount time always see fresh values.
  const showHiddenRef = useRef(showHidden)
  useEffect(() => { showHiddenRef.current = showHidden }, [showHidden])

  // setPendingDelete is a stable Preact state setter — safe to capture once.
  const setPendingDeleteRef = useRef(setPendingDelete)
  useEffect(() => { setPendingDeleteRef.current = setPendingDelete }, [setPendingDelete])

  // Tracks whether the next rename commit is a new-item creation.
  const pendingCreateTypeRef = useRef<'file' | 'dir' | null>(null)

  // Error banner with auto-dismiss.
  const errorTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const showError = useCallback((msg: string) => {
    if (errorTimerRef.current !== null) clearTimeout(errorTimerRef.current)
    setError(msg)
    errorTimerRef.current = setTimeout(() => setError(null), 4000)
  }, [])

  // ── Data fetchers ──

  const loadPaths = useCallback(async () => {
    try {
      const all = await apiWalkPaths(projectSlug)
      const filtered = filterPaths(all, showHiddenRef.current)
      treeRef.current?.resetPaths(filtered)
    } catch (e) {
      showError(String(e))
    }
  }, [projectSlug, showError])

  const refreshGitStatus = useCallback(async () => {
    try {
      const entries = await apiGitFiles(projectSlug)
      treeRef.current?.setGitStatus(entries)
    } catch {
      // git status errors are non-fatal — keep last known state
    }
  }, [projectSlug])

  // ── Mount the @pierre/trees vanilla FileTree ──

  useEffect(() => {
    const el = containerRef.current
    if (!el) return

    const tree = new PierreFileTree({
      paths: [],
      initialExpansion: 1,
      search: true,

      dragAndDrop: {
        onDropComplete: async event => {
          // `event.target.directoryPath` is null for root drops; treat as ''.
          const dir = event.target.directoryPath ?? ''
          await Promise.allSettled(
            event.draggedPaths.map(async fromPath => {
              const base = stripSlash(fromPath).split('/').pop() ?? stripSlash(fromPath)
              const toPath = dir ? `${dir}${base}` : base
              if (stripSlash(fromPath) !== toPath) {
                try {
                  await apiMove(projectSlug, stripSlash(fromPath), toPath)
                } catch (e) {
                  showError(String(e))
                }
              }
            }),
          )
          void loadPaths()
        },
      },

      renaming: {
        onRename: async event => {
          const src = stripSlash(event.sourcePath)
          const dest = stripSlash(event.destinationPath)
          const createType = pendingCreateTypeRef.current
          if (createType) {
            pendingCreateTypeRef.current = null
            try {
              if (createType === 'dir') {
                await apiMkdir(projectSlug, dest)
              } else {
                await apiCreateFile(projectSlug, dest)
                await apiOpen(projectSlug, dest)
              }
            } catch (e) {
              showError(String(e))
            }
          } else if (src !== dest) {
            try {
              await apiRename(projectSlug, src, dest)
            } catch (e) {
              showError(String(e))
            }
          }
          void loadPaths()
        },
      },

      onSelectionChange: selectedPaths => {
        const path = selectedPaths[0]
        // Skip empty selection or directory selection.
        if (!path || path.endsWith('/')) return
        if (path.toLowerCase().endsWith('.md')) {
          navigateToMarkdownEditor(projectSlug, path)
        } else if (/\.html?$/i.test(path)) {
          void apiOpenBrowser(projectSlug, path)
        } else if (/\.(png|jpe?g|gif|webp|svg|bmp|ico|tiff?|avif)$/i.test(path)) {
          navigateToImageViewer(projectSlug, path)
        } else {
          const existing = findOpenFileSession(sessions.value, cwd, path)
          if (existing) {
            navigateToSession(existing.id)
          } else {
            void apiOpen(projectSlug, path)
          }
        }
      },

      composition: {
        contextMenu: {
          enabled: true,
          triggerMode: 'both',
          render: (item, context) => {
            const menu = document.createElement('div')
            menu.className = 'ft-ctx-menu'
            // Mark as owned surface so the library doesn't treat inside-clicks
            // as outside clicks that dismiss the menu.
            menu.setAttribute('data-file-tree-context-menu-root', 'true')

            const addBtn = (label: string, className: string, action: () => void) => {
              const btn = document.createElement('button')
              btn.className = `ft-ctx-item${className ? ' ' + className : ''}`
              btn.textContent = label
              btn.onclick = action
              menu.appendChild(btn)
            }

            addBtn('Rename', '', () => {
              context.close({ restoreFocus: false })
              treeRef.current?.startRenaming(item.path)
            })

            addBtn('Copy path', '', () => {
              context.close()
              void copyRelativePath(stripSlash(item.path))
            })

            addBtn('Delete', 'ft-ctx-item--danger', () => {
              context.close()
              const name = stripSlash(item.path).split('/').pop() ?? item.path
              setPendingDeleteRef.current({
                path: item.path,
                name,
                isFolder: item.kind === 'directory',
              })
            })

            return menu
          },
        },
      },
    })

    tree.render({ containerWrapper: el })
    treeRef.current = tree

    void loadPaths()
    void refreshGitStatus()

    return () => {
      tree.cleanUp()
      treeRef.current = null
    }
    // Intentional: only re-mount when projectSlug changes (cwd follows projectSlug).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectSlug])

  // Re-filter paths when showHidden toggles.
  useEffect(() => {
    void loadPaths()
  }, [showHidden, loadPaths])

  // Poll: refresh paths + git status every 2.5 s.
  useEffect(() => {
    const id = setInterval(() => {
      void loadPaths()
      void refreshGitStatus()
    }, 2500)
    return () => clearInterval(id)
  }, [loadPaths, refreshGitStatus])

  // ── New item creation ──

  const handleAddStart = useCallback((type: 'file' | 'dir') => {
    const tree = treeRef.current
    if (!tree) return
    // Use a non-hidden placeholder so it's visible regardless of showHidden.
    const placeholder = type === 'dir' ? '__new-folder__/' : '__new-file__'
    pendingCreateTypeRef.current = type
    tree.add(placeholder)
    tree.startRenaming(placeholder, { removeIfCanceled: true })
  }, [])

  // ── Delete confirm ──

  const handleDeleteConfirm = useCallback(async () => {
    if (!pendingDelete) return
    const { path, isFolder } = pendingDelete
    setPendingDelete(null)
    try {
      await apiDelete(projectSlug, stripSlash(path), isFolder)
      await loadPaths()
    } catch (e) {
      showError(String(e))
    }
  }, [pendingDelete, projectSlug, loadPaths, showError])

  // ── External file drop onto the tree zone ──

  const handleRootDragOver = useCallback((e: DragEvent) => {
    if (!e.dataTransfer || e.dataTransfer.files.length === 0) return
    e.preventDefault()
    e.dataTransfer.dropEffect = 'copy'
    setExternalDragOver(true)
  }, [])

  const handleRootDrop = useCallback(
    async (e: DragEvent) => {
      e.preventDefault()
      setExternalDragOver(false)
      if (!e.dataTransfer || e.dataTransfer.files.length === 0) return
      try {
        await apiUpload(projectSlug, '', e.dataTransfer.files)
        await loadPaths()
      } catch (e2) {
        showError(String(e2))
      }
    },
    [projectSlug, loadPaths, showError],
  )

  const displayCwd = cwd.replace(/^\/Users\/[^/]+/, '~').replace(/^\/home\/[^/]+/, '~')

  return (
    <div class="ft-root">
      {/* Header */}
      <div class="ft-header">
        <span class="ft-header-label" title={cwd}>Files</span>
        <span class="ft-header-cwd" title={cwd}>{displayCwd}</span>
        <GitStatus projectSlug={projectSlug} cwd={cwd} />
        <button
          class={`ft-header-btn${showHidden ? ' ft-header-btn--active' : ''}`}
          title={showHidden ? 'Hide hidden files' : 'Show hidden files'}
          onClick={() => setShowHidden(v => !v)}
        >
          {showHidden ? <IconEye /> : <IconEyeOff />}
        </button>
        <button class="ft-header-btn" title="New file" onClick={() => handleAddStart('file')}>
          <IconPlus />
        </button>
        <button class="ft-header-btn" title="New folder" onClick={() => handleAddStart('dir')}>
          <IconFolderPlus />
        </button>
      </div>

      {/* Error banner */}
      {error && <div class="ft-error">{error}</div>}

      {/* Tree + external drop zone */}
      <div
        class={`ft-tree${externalDragOver ? ' ft-external-dragover' : ''}`}
        onDragOver={handleRootDragOver}
        onDragLeave={() => setExternalDragOver(false)}
        onDrop={handleRootDrop}
      >
        <div ref={containerRef} class="ft-tree-inner" />
      </div>

      {/* Delete confirmation modal */}
      {pendingDelete && (
        <div class="ft-modal-overlay" onClick={() => setPendingDelete(null)}>
          <div class="ft-modal" onClick={e => e.stopPropagation()}>
            <p class="ft-modal-title">
              Delete {pendingDelete.isFolder ? 'folder' : 'file'}?
            </p>
            <p class="ft-modal-body">
              <strong>{pendingDelete.name}</strong>
              {pendingDelete.isFolder && ' and all its contents'} will be permanently deleted.
            </p>
            <div class="ft-modal-actions">
              <button class="ft-modal-cancel" onClick={() => setPendingDelete(null)}>Cancel</button>
              <button class="ft-modal-confirm" onClick={handleDeleteConfirm}>Delete</button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
