/**
 * FileTree — filesystem browser panel for the sidebar.
 *
 * Uses @pierre/trees/react (useFileTree hook + <FileTree> component) to render
 * the file tree. The library owns its shadow DOM; this component handles:
 *   - GET /v1/fs/{slug}/walk   → paths fed to model.resetPaths()
 *   - GET /v1/git/{slug}/files  → per-file status fed to model.setGitStatus()
 *   - rename / move / delete / create callbacks → REST mutations + refresh
 *   - External file drop (Finder → upload)
 *   - Show-hidden toggle, new-file/new-folder buttons
 *   - Delete confirmation modal
 *
 * All filesystem mutations go through /v1/fs/{slug}/* REST endpoints.
 */

import { useState, useCallback, useRef, useEffect } from 'preact/hooks'
import { useFileTree, FileTree as PierreFileTree } from '@pierre/trees/react'
import type {
  GitStatusEntry,
  ContextMenuItem as FileTreeContextMenuItem,
  ContextMenuOpenContext as FileTreeContextMenuOpenContext,
  FileTreeDropResult,
  FileTreeRenameEvent,
} from '@pierre/trees'
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
 * Collect the paths of expanded directories by querying the model for each
 * directory path in the current walk response. Preserves expansion state
 * across resetPaths() calls so polling doesn't collapse user-opened folders.
 */
export function getExpandedPaths(
  model: { getItem(path: string): import('@pierre/trees').FileTreeItemHandle | null },
  dirPaths: string[],
): string[] {
  return dirPaths.filter(p => {
    const item = model.getItem(p)
    return item !== null && item.isDirectory() && (item as import('@pierre/trees').FileTreeDirectoryHandle).isExpanded()
  })
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

async function apiWalkPaths(slug: string, includeHidden: boolean, full = false): Promise<string[]> {
  const params = new URLSearchParams()
  if (includeHidden) params.set('include_hidden', 'true')
  if (full) params.set('full', 'true')
  const qs = params.size ? `?${params}` : ''
  const url = `/v1/fs/${encodeURIComponent(slug)}/walk${qs}`
  const resp = await apiFetch('GET', url)
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

// ── Helpers ─────────────────────────────────────────────────────────────────

/** Strip trailing slash from directory paths (for API calls). */
function stripSlash(p: string): string {
  return p.endsWith('/') ? p.slice(0, -1) : p
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

// ── Component ────────────────────────────────────────────────────────────────

export interface FileTreeProps {
  projectSlug: string
  /** Absolute filesystem path of the project root (used for display only). */
  cwd: string
  /** Called when the sidebar should close on mobile after a navigation. */
  onMobileClose?: () => void
}

export function FileTree({ projectSlug, cwd }: FileTreeProps) {
  const [showHidden, setShowHidden] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [externalDragOver, setExternalDragOver] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<{
    path: string
    name: string
    isFolder: boolean
  } | null>(null)

  // Keep fresh values accessible from stable model callbacks via refs.
  const showHiddenRef = useRef(showHidden)
  const projectSlugRef = useRef(projectSlug)
  const cwdRef = useRef(cwd)
  useEffect(() => { showHiddenRef.current = showHidden }, [showHidden])
  useEffect(() => { projectSlugRef.current = projectSlug }, [projectSlug])
  useEffect(() => { cwdRef.current = cwd }, [cwd])

  // Tracks whether the next rename commit is a new-item creation.
  const pendingCreateTypeRef = useRef<'file' | 'dir' | null>(null)

  // Guard: suppress onSelectionChange callbacks that fire when resetPaths()
  // restores the previously-selected path after a reload (every 2.5s poll).
  // Without this guard, apiOpen fires on every polling cycle.
  const resettingPathsRef = useRef(false)

  // Error banner with auto-dismiss.
  const errorTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const showError = useCallback((msg: string) => {
    if (errorTimerRef.current !== null) clearTimeout(errorTimerRef.current)
    setError(msg)
    errorTimerRef.current = setTimeout(() => setError(null), 4000)
  }, [])
  const showErrorRef = useRef(showError)
  useEffect(() => { showErrorRef.current = showError }, [showError])

  // ── Data fetchers ──

  // Stable model ref — set immediately since useFileTree creates the model
  // synchronously in useState(); updated after each render just in case.
  const modelRef = useRef<ReturnType<typeof useFileTree>['model'] | null>(null)

  const loadPaths = useCallback(async () => {
    const model = modelRef.current
    if (!model) return
    try {
      // Fast walk: depth 3, no bulk dirs — renders the tree immediately.
      const paths = await apiWalkPaths(projectSlugRef.current, showHiddenRef.current)
      const dirPaths = paths.filter(p => p.endsWith('/'))
      const expandedPaths = getExpandedPaths(model, dirPaths)
      resettingPathsRef.current = true
      setTimeout(() => { resettingPathsRef.current = false }, 0)
      model.resetPaths(paths, {
        initialExpandedPaths: expandedPaths,
      })

      // Full walk: stream in the rest (node_modules etc.) in the background.
      // Uses batch() so the tree stays interactive while new paths arrive.
      apiWalkPaths(projectSlugRef.current, showHiddenRef.current, true)
        .then(fullPaths => {
          const current = new Set(paths)
          const additions = fullPaths.filter(p => !current.has(p))
          if (additions.length > 0) {
            modelRef.current?.batch(additions.map(p => ({ type: 'add' as const, path: p })))
          }
        })
        .catch(e => console.warn('[gmux] file-tree: full walk failed', e))
    } catch (e) {
      console.error('[gmux] file-tree: walk failed for slug', projectSlugRef.current, e)
      showErrorRef.current(String(e))
    }
  }, [])

  const refreshGitStatus = useCallback(async () => {
    const model = modelRef.current
    if (!model) return
    try {
      const entries = await apiGitFiles(projectSlugRef.current)
      model.setGitStatus(entries)
    } catch {
      // git status errors are non-fatal — keep last known state
    }
  }, [])

  // Stable refs for event callbacks passed to useFileTree (called by the
  // library; need to see the latest loadPaths / showError without
  // re-creating the model).
  const loadPathsRef   = useRef(loadPaths)
  const refreshGitStatusRef = useRef(refreshGitStatus)
  useEffect(() => { loadPathsRef.current = loadPaths }, [loadPaths])
  useEffect(() => { refreshGitStatusRef.current = refreshGitStatus }, [refreshGitStatus])

  // ── Build the @pierre/trees model via hook ──
  // Options are stable wrappers that call the latest ref — model is created
  // only once regardless of projectSlug changes (projectSlug is read from ref).

  const { model } = useFileTree({
    paths: [],
    initialExpansion: 0,  // start collapsed; auto-expanding races with expand/collapse tests
    search: true,

    dragAndDrop: {
      onDropComplete: async (event: FileTreeDropResult) => {
        const dir = event.target.directoryPath ?? ''
        await Promise.allSettled(
          event.draggedPaths.map(async fromPath => {
            const base = stripSlash(fromPath).split('/').pop() ?? stripSlash(fromPath)
            const toPath = dir ? `${dir}${base}` : base
            if (stripSlash(fromPath) !== toPath) {
              try {
                await apiMove(projectSlugRef.current, stripSlash(fromPath), toPath)
              } catch (e) {
                showErrorRef.current(String(e))
              }
            }
          }),
        )
        void loadPathsRef.current()
      },
    },

    renaming: {
      onRename: async (event: FileTreeRenameEvent) => {
        const src = stripSlash(event.sourcePath)
        const dest = stripSlash(event.destinationPath)
        const createType = pendingCreateTypeRef.current
        if (createType) {
          pendingCreateTypeRef.current = null
          try {
            if (createType === 'dir') {
              await apiMkdir(projectSlugRef.current, dest)
            } else {
              await apiCreateFile(projectSlugRef.current, dest)
              await apiOpen(projectSlugRef.current, dest)
            }
          } catch (e) {
            showErrorRef.current(String(e))
          }
        } else if (src !== dest) {
          try {
            await apiRename(projectSlugRef.current, src, dest)
          } catch (e) {
            showErrorRef.current(String(e))
          }
        }
        void loadPathsRef.current()
      },
    },

    onSelectionChange: (selectedPaths: readonly string[]) => {
      // Ignore auto-selections triggered by resetPaths() restoring the
      // previously selected path — only act on explicit user interactions.
      if (resettingPathsRef.current) return
      const path = selectedPaths[0]
      if (!path || path.endsWith('/')) return
      const slug = projectSlugRef.current
      const cwd  = cwdRef.current
      if (path.toLowerCase().endsWith('.md')) {
        navigateToMarkdownEditor(slug, path)
      } else if (/\.html?$/i.test(path)) {
        void apiOpenBrowser(slug, path)
      } else if (/\.(png|jpe?g|gif|webp|svg|bmp|ico|tiff?|avif)$/i.test(path)) {
        navigateToImageViewer(slug, path)
      } else {
        const existing = findOpenFileSession(sessions.value, cwd, path)
        if (existing) {
          navigateToSession(existing.id)
        } else {
          void apiOpen(slug, path)
        }
      }
    },
  })

  // Keep modelRef in sync (model is stable but this is defensive).
  modelRef.current = model

  // ── Initial load + projectSlug change: reset paths when slug changes ──
  useEffect(() => {
    void loadPaths()
    void refreshGitStatus()
  }, [projectSlug, loadPaths, refreshGitStatus])

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
    const placeholder = type === 'dir' ? '__new-folder__/' : '__new-file__'
    pendingCreateTypeRef.current = type
    // Suppress the auto-selection that tree.add()+startRenaming() triggers.
    resettingPathsRef.current = true
    setTimeout(() => { resettingPathsRef.current = false }, 0)
    model.add(placeholder)
    model.startRenaming(placeholder, { removeIfCanceled: true })
  }, [model])

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

  // ── Context menu (rendered as Preact JSX via renderContextMenu prop) ──

  const renderContextMenu = useCallback(
    (item: FileTreeContextMenuItem, context: FileTreeContextMenuOpenContext) => (
      <div class="ft-ctx-menu">
        <button
          class="ft-ctx-item"
          onClick={() => {
            context.close({ restoreFocus: false })
            model.startRenaming(item.path)
          }}
        >
          Rename
        </button>
        <button
          class="ft-ctx-item"
          onClick={() => {
            context.close()
            void copyRelativePath(stripSlash(item.path))
          }}
        >
          Copy path
        </button>
        <button
          class="ft-ctx-item ft-ctx-item--danger"
          onClick={() => {
            context.close()
            const name = stripSlash(item.path).split('/').pop() ?? item.path
            setPendingDelete({
              path: item.path,
              name,
              isFolder: item.kind === 'directory',
            })
          }}
        >
          Delete
        </button>
      </div>
    ),
    [model],
  )

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
        <div class="ft-tree-inner">
          <PierreFileTree
            model={model}
            renderContextMenu={renderContextMenu}
          />
        </div>
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
