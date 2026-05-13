/**
 * FileTree — filesystem browser panel for the sidebar.
 *
 * Shows the project root directory as a tree. Supports:
 *   - Lazy-load children on expand
 *   - Drag/drop to move within the tree
 *   - Drop files from external apps (dataTransfer.files)
 *   - Inline rename (pencil icon)
 *   - Add new file (inline name input → create + open in editor)
 *   - Add new folder (inline name input → mkdir)
 *   - Delete with confirmation modal
 *
 * All filesystem mutations go through /v1/fs/{slug}/* REST endpoints.
 * State is local to this component; no global store mutations.
 */

import { useState, useCallback, useRef, useEffect } from 'preact/hooks'

// ── Types ──

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

// ── Pure helpers (exported for tests) ──

/** Normalize a relative path: strip leading slashes, collapse redundant separators. */
export function normalizeFsPath(rel: string): string {
  return rel.replace(/^\/+/, '').replace(/\/+/g, '/').replace(/\/$/, '')
}

/** Join two relative path segments. */
export function joinFsPath(parent: string, name: string): string {
  if (!parent) return name
  return `${parent}/${name}`
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
 * Returns true if a DragEvent carries external files (e.g. from Finder/Explorer).
 * This distinguishes OS-file drops from internal tree drags.
 */
export function isExternalFileDrop(dt: DataTransfer): boolean {
  return dt.files.length > 0
}

// ── API helpers ──

async function apiFetch(method: string, url: string, body?: unknown): Promise<{ ok: boolean; data?: unknown; error?: { code: string; message: string } }> {
  const opts: RequestInit = { method }
  if (body !== undefined) {
    opts.headers = { 'Content-Type': 'application/json' }
    opts.body = JSON.stringify(body)
  }
  const resp = await fetch(url, opts)
  return resp.json()
}

async function listDir(slug: string, rel: string): Promise<FileEntry[]> {
  const url = `/v1/fs/${encodeURIComponent(slug)}?path=${encodeURIComponent(rel)}`
  const resp = await apiFetch('GET', url)
  if (!resp.ok) throw new Error((resp.error?.message) ?? 'list failed')
  return resp.data as FileEntry[]
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
  const resp = await apiFetch('DELETE', `/v1/fs/${encodeURIComponent(slug)}/item`, { path, recursive })
  if (!resp.ok) throw new Error((resp.error?.message) ?? 'delete failed')
}

async function apiOpen(slug: string, path: string): Promise<void> {
  const resp = await apiFetch('POST', `/v1/fs/${encodeURIComponent(slug)}/open`, { path })
  if (!resp.ok) throw new Error((resp.error?.message) ?? 'open failed')
}

async function apiUpload(slug: string, dir: string, files: FileList): Promise<void> {
  const form = new FormData()
  for (let i = 0; i < files.length; i++) {
    form.append('file', files[i])
  }
  const resp = await fetch(
    `/v1/fs/${encodeURIComponent(slug)}/upload?dir=${encodeURIComponent(dir)}`,
    { method: 'POST', body: form },
  )
  const json = await resp.json()
  if (!json.ok) throw new Error((json.error?.message) ?? 'upload failed')
}

// ── Icons ──

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

const IconChevronRight = () => (
  <svg {...svgProps} class="ft-chevron"><path d="M4 3l3 3-3 3"/></svg>
)
const IconChevronDown = () => (
  <svg {...svgProps} class="ft-chevron"><path d="M3 4l3 3 3-3"/></svg>
)
const IconFile = () => (
  <svg {...svgProps} class="ft-icon ft-icon-file"><path d="M2 1h6l2 2v8H2z"/><path d="M7 1v3h3"/></svg>
)
const IconFolder = ({ open }: { open?: boolean }) => (
  <svg {...svgProps} class="ft-icon ft-icon-folder">
    {open
      ? <><path d="M1 4h10v7H1z"/><path d="M1 4l1.5-2H5l1 1.5H1"/></>
      : <><path d="M1 4h10v7H1z"/><path d="M1 4l1.5-2H5l1 1.5H1"/></>
    }
  </svg>
)
const IconPencil = () => (
  <svg {...svgProps} class="ft-action-icon"><path d="M8 2l2 2-6 6H2V8z"/><path d="M7 3l2 2"/></svg>
)
const IconTrash = () => (
  <svg {...svgProps} class="ft-action-icon"><path d="M2 3h8M5 3V2h2v1M4 3v7h4V3"/><path d="M5 5v3M7 5v3"/></svg>
)
const IconPlus = () => (
  <svg {...svgProps} class="ft-action-icon"><path d="M6 2v8M2 6h8"/></svg>
)
const IconFolderPlus = () => (
  <svg {...svgProps} class="ft-action-icon"><path d="M1 4h10v7H1z"/><path d="M1 4l1.5-2H5l1 1.5H1"/><path d="M6 8V6M5 7h2"/></svg>
)

// ── Inline input for naming new files/folders or renaming ──

function InlineInput({
  initial,
  onCommit,
  onCancel,
}: {
  initial: string
  onCommit: (value: string) => void
  onCancel: () => void
}) {
  const [value, setValue] = useState(initial)
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    inputRef.current?.focus()
    inputRef.current?.select()
  }, [])

  const commit = useCallback(() => {
    const trimmed = value.trim()
    if (trimmed) onCommit(trimmed)
    else onCancel()
  }, [value, onCommit, onCancel])

  return (
    <input
      ref={inputRef}
      class="ft-inline-input"
      value={value}
      onInput={e => setValue((e.target as HTMLInputElement).value)}
      onKeyDown={e => {
        if (e.key === 'Enter') { e.preventDefault(); commit() }
        if (e.key === 'Escape') { e.preventDefault(); onCancel() }
      }}
      onBlur={commit}
      onClick={e => e.stopPropagation()}
    />
  )
}

// ── Delete confirmation modal ──

function DeleteModal({
  node,
  onConfirm,
  onCancel,
}: {
  node: TreeNode
  onConfirm: () => void
  onCancel: () => void
}) {
  return (
    <div class="ft-modal-overlay" onClick={onCancel}>
      <div class="ft-modal" onClick={e => e.stopPropagation()}>
        <p class="ft-modal-title">
          Delete {node.type === 'dir' ? 'folder' : 'file'}?
        </p>
        <p class="ft-modal-body">
          <strong>{node.name}</strong>
          {node.type === 'dir' && ' and all its contents'} will be permanently deleted.
        </p>
        <div class="ft-modal-actions">
          <button class="ft-modal-cancel" onClick={onCancel}>Cancel</button>
          <button class="ft-modal-confirm" onClick={onConfirm}>Delete</button>
        </div>
      </div>
    </div>
  )
}

// ── Adding state ──

interface AddingState {
  parentPath: string  // relative path of the parent dir
  type: 'file' | 'dir'
}

// ── FileTreeNode ──

interface FileTreeNodeProps {
  node: TreeNode
  slug: string
  depth: number
  expanded: Set<string>
  childCache: Map<string, TreeNode[]>
  adding: AddingState | null
  renamingPath: string | null
  dragSource: string | null
  dropTarget: string | null
  onToggle: (path: string) => void
  onLoad: (path: string) => Promise<void>
  onRenameStart: (path: string) => void
  onRenameCommit: (node: TreeNode, newName: string) => Promise<void>
  onRenameCancel: () => void
  onDeleteRequest: (node: TreeNode) => void
  onDragStart: (path: string) => void
  onDragOver: (e: DragEvent, targetPath: string, targetType: 'file' | 'dir') => void
  onDrop: (e: DragEvent, targetPath: string, targetType: 'file' | 'dir') => void
  onDragEnd: () => void
  onAddCommit: (name: string) => Promise<void>
  onAddCancel: () => void
}

function FileTreeNode({
  node,
  slug,
  depth,
  expanded,
  childCache,
  adding,
  renamingPath,
  dragSource,
  dropTarget,
  onToggle,
  onLoad,
  onRenameStart,
  onRenameCommit,
  onRenameCancel,
  onDeleteRequest,
  onDragStart,
  onDragOver,
  onDrop,
  onDragEnd,
  onAddCommit,
  onAddCancel,
}: FileTreeNodeProps) {
  const isExpanded = node.type === 'dir' && expanded.has(node.path)
  const children = childCache.get(node.path) ?? []
  const isDropTarget = dropTarget === node.path
  const isDragging = dragSource === node.path
  const isRenaming = renamingPath === node.path

  const indent = depth * 12

  const handleClick = useCallback(async () => {
    if (node.type === 'dir') {
      onToggle(node.path)
      if (!expanded.has(node.path) && !childCache.has(node.path)) {
        await onLoad(node.path)
      }
    } else {
      await apiOpen(slug, node.path)
    }
  }, [node, slug, expanded, childCache, onToggle, onLoad])

  const addingInThisDir = adding?.parentPath === node.path

  return (
    <div class="ft-node-group">
      <div
        class={[
          'ft-node',
          isDropTarget ? 'ft-drop-target' : '',
          isDragging ? 'ft-dragging' : '',
        ].filter(Boolean).join(' ')}
        style={{ paddingLeft: `${8 + indent}px` }}
        draggable
        onDragStart={e => {
          e.dataTransfer!.effectAllowed = 'move'
          e.dataTransfer!.setData('text/plain', node.path)
          onDragStart(node.path)
        }}
        onDragOver={e => onDragOver(e, node.path, node.type)}
        onDrop={e => onDrop(e, node.path, node.type)}
        onDragEnd={onDragEnd}
      >
        {/* Chevron for dirs */}
        <span class="ft-chevron-wrap" onClick={handleClick}>
          {node.type === 'dir'
            ? (isExpanded ? <IconChevronDown /> : <IconChevronRight />)
            : <span class="ft-chevron-spacer" />
          }
        </span>

        {/* Icon */}
        {node.type === 'dir'
          ? <IconFolder open={isExpanded} />
          : <IconFile />
        }

        {/* Name or inline rename input */}
        {isRenaming
          ? (
            <InlineInput
              initial={node.name}
              onCommit={newName => onRenameCommit(node, newName)}
              onCancel={onRenameCancel}
            />
          )
          : (
            <span class="ft-node-name" onClick={handleClick} title={node.path}>
              {node.name}
            </span>
          )
        }

        {/* Action buttons (hover-visible) */}
        {!isRenaming && (
          <span class="ft-node-actions">
            <button
              class="ft-action-btn"
              title="Rename"
              onClick={e => { e.stopPropagation(); onRenameStart(node.path) }}
            >
              <IconPencil />
            </button>
            <button
              class="ft-action-btn ft-action-delete"
              title="Delete"
              onClick={e => { e.stopPropagation(); onDeleteRequest(node) }}
            >
              <IconTrash />
            </button>
          </span>
        )}
      </div>

      {/* Children (when expanded) */}
      {node.type === 'dir' && isExpanded && (
        <div class="ft-children">
          {children.map(child => (
            <FileTreeNode
              key={child.path}
              node={child}
              slug={slug}
              depth={depth + 1}
              expanded={expanded}
              childCache={childCache}
              adding={adding}
              renamingPath={renamingPath}
              dragSource={dragSource}
              dropTarget={dropTarget}
              onToggle={onToggle}
              onLoad={onLoad}
              onRenameStart={onRenameStart}
              onRenameCommit={onRenameCommit}
              onRenameCancel={onRenameCancel}
              onDeleteRequest={onDeleteRequest}
              onDragStart={onDragStart}
              onDragOver={onDragOver}
              onDrop={onDrop}
              onDragEnd={onDragEnd}
              onAddCommit={onAddCommit}
              onAddCancel={onAddCancel}
            />
          ))}
          {/* Inline add input appears at bottom of expanded dir */}
          {addingInThisDir && (
            <div class="ft-node ft-node-adding" style={{ paddingLeft: `${8 + (depth + 1) * 12}px` }}>
              {adding!.type === 'dir' ? <IconFolderPlus /> : <IconFile />}
              <InlineInput
                initial=""
                onCommit={onAddCommit}
                onCancel={onAddCancel}
              />
            </div>
          )}
        </div>
      )}
    </div>
  )
}

// ── FileTree (root component) ──

export interface FileTreeProps {
  projectSlug: string
  /** Absolute filesystem path of the project root (used for display only). */
  cwd: string
  /** Called when the sidebar should close on mobile after a navigation. */
  onMobileClose?: () => void
}

export function FileTree({ projectSlug, cwd }: FileTreeProps) {
  const [rootNodes, setRootNodes] = useState<TreeNode[]>([])
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  // childCache is a ref (not state) so updates don't re-render the whole tree.
  // We use a counter to force re-render after mutations.
  const childCacheRef = useRef<Map<string, TreeNode[]>>(new Map())
  const [cacheVersion, setCacheVersion] = useState(0)
  const bumpCache = useCallback(() => setCacheVersion(v => v + 1), [])

  const [renamingPath, setRenamingPath] = useState<string | null>(null)
  const [dragSource, setDragSource] = useState<string | null>(null)
  const [dropTarget, setDropTarget] = useState<string | null>(null)
  const [pendingDelete, setPendingDelete] = useState<TreeNode | null>(null)
  const [adding, setAdding] = useState<AddingState | null>(null)
  const [error, setError] = useState<string | null>(null)

  // External file drop on the root zone
  const [externalDragOver, setExternalDragOver] = useState(false)

  const showError = useCallback((msg: string) => {
    setError(msg)
    setTimeout(() => setError(null), 4000)
  }, [])

  // Load root on mount and after mutations
  const loadRoot = useCallback(async () => {
    try {
      const entries = await listDir(projectSlug, '')
      setRootNodes(buildTreeNodes(entries, ''))
    } catch (e) {
      showError(String(e))
    }
  }, [projectSlug, showError])

  useEffect(() => { void loadRoot() }, [loadRoot])

  // Load children for a directory node
  const loadChildren = useCallback(async (dirPath: string) => {
    try {
      const entries = await listDir(projectSlug, dirPath)
      childCacheRef.current.set(dirPath, buildTreeNodes(entries, dirPath))
      bumpCache()
    } catch (e) {
      showError(String(e))
    }
  }, [projectSlug, showError, bumpCache])

  // Refresh a directory (re-load its children and the root if needed)
  const refreshDir = useCallback(async (dirPath: string) => {
    if (dirPath === '') {
      await loadRoot()
    } else {
      await loadChildren(dirPath)
    }
  }, [loadRoot, loadChildren])

  // Toggle expand/collapse
  const handleToggle = useCallback((path: string) => {
    setExpanded(prev => {
      const next = new Set(prev)
      if (next.has(path)) next.delete(path)
      else next.add(path)
      return next
    })
  }, [])

  // Rename
  const handleRenameCommit = useCallback(async (node: TreeNode, newName: string) => {
    if (newName === node.name) { setRenamingPath(null); return }
    const parentPath = node.path.includes('/')
      ? node.path.slice(0, node.path.lastIndexOf('/'))
      : ''
    const toPath = joinFsPath(parentPath, newName)
    try {
      await apiRename(projectSlug, node.path, toPath)
      setRenamingPath(null)
      // Invalidate parent dir cache
      childCacheRef.current.delete(parentPath)
      await refreshDir(parentPath)
    } catch (e) {
      showError(String(e))
      setRenamingPath(null)
    }
  }, [projectSlug, showError, refreshDir])

  // Drag/drop (internal)
  const handleDragOver = useCallback((e: DragEvent, targetPath: string, targetType: 'file' | 'dir') => {
    // Only handle internal drags (no files from OS)
    if (e.dataTransfer && isExternalFileDrop(e.dataTransfer)) return
    e.preventDefault()
    e.dataTransfer!.dropEffect = 'move'
    // Can only drop onto dirs
    if (targetType === 'dir') setDropTarget(targetPath)
  }, [])

  const handleDrop = useCallback(async (e: DragEvent, targetPath: string, targetType: 'file' | 'dir') => {
    e.preventDefault()
    const dt = e.dataTransfer!

    // External file drop
    if (isExternalFileDrop(dt)) {
      // If dropped on a collapsed dir, expand it first
      if (targetType === 'dir' && !expanded.has(targetPath)) {
        setExpanded(prev => { const n = new Set(prev); n.add(targetPath); return n })
        if (!childCacheRef.current.has(targetPath)) {
          await loadChildren(targetPath)
        }
      }
      const dir = targetType === 'dir' ? targetPath : (
        targetPath.includes('/') ? targetPath.slice(0, targetPath.lastIndexOf('/')) : ''
      )
      try {
        await apiUpload(projectSlug, dir, dt.files)
        childCacheRef.current.delete(dir)
        await refreshDir(dir)
      } catch (e2) {
        showError(String(e2))
      }
      setDropTarget(null)
      return
    }

    // Internal move
    const fromPath = dt.getData('text/plain') || dragSource
    setDragSource(null)
    setDropTarget(null)
    if (!fromPath || fromPath === targetPath) return
    if (targetType !== 'dir') return

    try {
      await apiMove(projectSlug, fromPath, targetPath)
      // Expand the target dir
      setExpanded(prev => { const n = new Set(prev); n.add(targetPath); return n })
      // Refresh: source's parent and the target dir
      const srcParent = fromPath.includes('/')
        ? fromPath.slice(0, fromPath.lastIndexOf('/'))
        : ''
      childCacheRef.current.delete(srcParent)
      childCacheRef.current.delete(targetPath)
      await refreshDir(srcParent)
      await refreshDir(targetPath)
    } catch (e2) {
      showError(String(e2))
    }
  }, [projectSlug, dragSource, expanded, loadChildren, refreshDir, showError])

  // Delete
  const handleDeleteConfirm = useCallback(async () => {
    if (!pendingDelete) return
    const node = pendingDelete
    setPendingDelete(null)
    try {
      await apiDelete(projectSlug, node.path, node.type === 'dir')
      const parentPath = node.path.includes('/')
        ? node.path.slice(0, node.path.lastIndexOf('/'))
        : ''
      childCacheRef.current.delete(parentPath)
      childCacheRef.current.delete(node.path)
      await refreshDir(parentPath)
    } catch (e) {
      showError(String(e))
    }
  }, [projectSlug, pendingDelete, refreshDir, showError])

  // Add new file/folder
  const handleAddStart = useCallback(async (type: 'file' | 'dir') => {
    // Add at root level (parentPath = '')
    const parentPath = ''
    // Ensure root is "expanded" so the inline input is visible
    setAdding({ parentPath, type })
  }, [])

  const handleAddCommit = useCallback(async (name: string) => {
    if (!adding) return
    const newPath = joinFsPath(adding.parentPath, name)
    try {
      if (adding.type === 'dir') {
        await apiMkdir(projectSlug, newPath)
        setAdding(null)
        childCacheRef.current.delete(adding.parentPath)
        await refreshDir(adding.parentPath)
      } else {
        await apiCreateFile(projectSlug, newPath)
        setAdding(null)
        childCacheRef.current.delete(adding.parentPath)
        await refreshDir(adding.parentPath)
        await apiOpen(projectSlug, newPath)
      }
    } catch (e) {
      showError(String(e))
      setAdding(null)
    }
  }, [projectSlug, adding, refreshDir, showError])

  // External drag-over on root zone
  const handleRootDragOver = useCallback((e: DragEvent) => {
    if (!e.dataTransfer) return
    if (isExternalFileDrop(e.dataTransfer)) {
      e.preventDefault()
      e.dataTransfer.dropEffect = 'copy'
      setExternalDragOver(true)
    }
  }, [])

  const handleRootDrop = useCallback(async (e: DragEvent) => {
    e.preventDefault()
    setExternalDragOver(false)
    if (!e.dataTransfer || !isExternalFileDrop(e.dataTransfer)) return
    try {
      await apiUpload(projectSlug, '', e.dataTransfer.files)
      await loadRoot()
    } catch (e2) {
      showError(String(e2))
    }
  }, [projectSlug, loadRoot, showError])

  // Short display path for the header
  const displayCwd = cwd.replace(/^\/Users\/[^/]+/, '~').replace(/^\/home\/[^/]+/, '~')

  return (
    <div class="ft-root">
      {/* Header */}
      <div class="ft-header">
        <span class="ft-header-label" title={cwd}>Files</span>
        <span class="ft-header-cwd" title={cwd}>{displayCwd}</span>
        <button
          class="ft-header-btn"
          title="New file"
          onClick={() => handleAddStart('file')}
        >
          <IconPlus />
        </button>
        <button
          class="ft-header-btn"
          title="New folder"
          onClick={() => handleAddStart('dir')}
        >
          <IconFolderPlus />
        </button>
      </div>

      {/* Error banner */}
      {error && <div class="ft-error">{error}</div>}

      {/* Tree */}
      <div
        class={`ft-tree${externalDragOver ? ' ft-external-dragover' : ''}`}
        onDragOver={handleRootDragOver}
        onDragLeave={() => setExternalDragOver(false)}
        onDrop={handleRootDrop}
      >
        {rootNodes.map(node => (
          <FileTreeNode
            key={node.path}
            node={node}
            slug={projectSlug}
            depth={0}
            expanded={expanded}
            childCache={childCacheRef.current}
            adding={adding}
            renamingPath={renamingPath}
            dragSource={dragSource}
            dropTarget={dropTarget}
            onToggle={handleToggle}
            onLoad={loadChildren}
            onRenameStart={setRenamingPath}
            onRenameCommit={handleRenameCommit}
            onRenameCancel={() => setRenamingPath(null)}
            onDeleteRequest={setPendingDelete}
            onDragStart={setDragSource}
            onDragOver={handleDragOver}
            onDrop={handleDrop}
            onDragEnd={() => { setDragSource(null); setDropTarget(null) }}
            onAddCommit={handleAddCommit}
            onAddCancel={() => setAdding(null)}
          />
        ))}

        {/* Root-level inline add input (when adding at root) */}
        {adding && adding.parentPath === '' && (
          <div class="ft-node ft-node-adding" style={{ paddingLeft: '8px' }}>
            {adding.type === 'dir' ? <IconFolderPlus /> : <IconFile />}
            <InlineInput
              initial=""
              onCommit={handleAddCommit}
              onCancel={() => setAdding(null)}
            />
          </div>
        )}

        {rootNodes.length === 0 && !adding && (
          <div class="ft-empty">Drop files here or click + to add</div>
        )}
      </div>

      {/* Delete confirmation modal */}
      {pendingDelete && (
        <DeleteModal
          node={pendingDelete}
          onConfirm={handleDeleteConfirm}
          onCancel={() => setPendingDelete(null)}
        />
      )}

      {/* Invisible version tracker so cacheVersion is used */}
      <span data-cache-version={cacheVersion} style={{ display: 'none' }} />
    </div>
  )
}
