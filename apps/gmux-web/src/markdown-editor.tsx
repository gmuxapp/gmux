/**
 * MarkdownEditor — TipTap v3 WYSIWYG editor for .md files.
 *
 * Opened when the user clicks a .md file in the file tree.
 * Reads content via GET /v1/fs/{slug}/read, writes back via POST /v1/fs/{slug}/write.
 * Auto-saves 1.5 s after the last edit, with Cmd/Ctrl+S as an explicit trigger.
 */

import { useEffect, useRef, useCallback, useState } from 'preact/hooks'
import '@fontsource/lora/400.css'
import '@fontsource/lora/700.css'
import { Editor } from '@tiptap/core'
import { StarterKit } from '@tiptap/starter-kit'
import { Markdown } from '@tiptap/markdown'
import { Table, TableRow, TableCell, TableHeader } from '@tiptap/extension-table'
import { TaskList } from '@tiptap/extension-task-list'
import { TaskItem } from '@tiptap/extension-task-item'

// ── API helpers ──────────────────────────────────────────────────────────────

async function apiReadFile(slug: string, path: string): Promise<string> {
  const resp = await fetch(
    `/v1/fs/${encodeURIComponent(slug)}/read?path=${encodeURIComponent(path)}`,
  )
  const json = await resp.json()
  if (!json.ok) throw new Error(json.error?.message ?? 'read failed')
  return (json.data as { content: string }).content
}

async function apiWriteFile(slug: string, path: string, content: string): Promise<void> {
  const resp = await fetch(`/v1/fs/${encodeURIComponent(slug)}/write`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path, content }),
  })
  const json = await resp.json()
  if (!json.ok) throw new Error(json.error?.message ?? 'write failed')
}

// ── Frontmatter helpers ──────────────────────────────────────────────────────

interface FrontmatterField {
  key: string
  value: string
}

/** Parse YAML frontmatter string (including --- delimiters) into key-value pairs.
 *  Handles simple `key: value` lines; skips blank lines and comments. */
function parseFrontmatterFields(raw: string): FrontmatterField[] {
  // raw = '---\n{content}\n---' (with or without trailing newline before ---)
  const inner = raw.slice(4) // strip leading '---\n'
  const end = inner.lastIndexOf('---')
  const body = end >= 0 ? inner.slice(0, end) : inner
  const fields: FrontmatterField[] = []
  for (const line of body.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) continue
    const colonIdx = trimmed.indexOf(':')
    if (colonIdx < 0) continue
    const key = trimmed.slice(0, colonIdx).trim()
    const value = trimmed.slice(colonIdx + 1).trim()
    fields.push({ key, value })
  }
  return fields
}

/** Reconstruct frontmatter string from key-value pairs. Returns null when empty. */
function fieldsToFrontmatter(fields: FrontmatterField[]): string | null {
  if (fields.length === 0) return null
  const lines = fields.map(({ key, value }) => `${key}: ${value}`)
  return '---\n' + lines.join('\n') + '\n---'
}

// ── Frontmatter editor component ─────────────────────────────────────────────

function FrontmatterEditor({
  fields,
  onChange,
}: {
  fields: FrontmatterField[]
  onChange: (fields: FrontmatterField[]) => void
}) {
  return (
    <div class="md-frontmatter-editor">
      {fields.map((f, i) => (
        <div class="fm-field" key={i}>
          <span class="fm-key">{f.key}</span>
          <span class="fm-sep">:</span>
          <input
            class="fm-value"
            value={f.value}
            placeholder="value"
            onInput={(e) => {
              const next = [...fields]
              next[i] = { ...f, value: (e.target as HTMLInputElement).value }
              onChange(next)
            }}
          />
          <button
            class="fm-delete"
            type="button"
            onClick={() => onChange(fields.filter((_, j) => j !== i))}
            title="Remove field"
          >×</button>
        </div>
      ))}
      <button
        class="fm-add"
        type="button"
        onClick={() => onChange([...fields, { key: 'new_field', value: '' }])}
      >+ field</button>
    </div>
  )
}

function parseFrontmatter(content: string): { frontmatter: string | null; body: string } {
  if (!content.startsWith('---\n') && !content.startsWith('---\r\n')) {
    return { frontmatter: null, body: content }
  }
  const rest = content.slice(4)
  const end = rest.match(/^(---|\.\.\.)[ \t]*$/m)
  if (!end || end.index === undefined) return { frontmatter: null, body: content }
  const fmEnd = end.index + end[0].length
  return {
    frontmatter: '---\n' + rest.slice(0, fmEnd),
    body: rest.slice(fmEnd).replace(/^\r?\n/, ''),
  }
}

// ── Toolbar ───────────────────────────────────────────────────────────────────

function Toolbar({ editor }: { editor: Editor | null }) {
  if (!editor) return null
  const btn = (label: string, action: () => void, active?: boolean, title?: string) => (
    <button
      class={`md-toolbar-btn${active ? ' active' : ''}`}
      type="button"
      title={title ?? label}
      onMouseDown={(e: MouseEvent) => { e.preventDefault(); action() }}
    >
      {label}
    </button>
  )
  return (
    <div class="md-toolbar">
      {btn('B', () => editor.chain().focus().toggleBold().run(), editor.isActive('bold'), 'Bold (⌘B)')}
      {btn('I', () => editor.chain().focus().toggleItalic().run(), editor.isActive('italic'), 'Italic (⌘I)')}
      {btn('S̶', () => editor.chain().focus().toggleStrike().run(), editor.isActive('strike'), 'Strikethrough')}
      {btn('`', () => editor.chain().focus().toggleCode().run(), editor.isActive('code'), 'Inline code')}
      <span class="md-toolbar-sep" />
      {btn('H1', () => editor.chain().focus().toggleHeading({ level: 1 }).run(), editor.isActive('heading', { level: 1 }))}
      {btn('H2', () => editor.chain().focus().toggleHeading({ level: 2 }).run(), editor.isActive('heading', { level: 2 }))}
      {btn('H3', () => editor.chain().focus().toggleHeading({ level: 3 }).run(), editor.isActive('heading', { level: 3 }))}
      <span class="md-toolbar-sep" />
      {btn('•', () => editor.chain().focus().toggleBulletList().run(), editor.isActive('bulletList'), 'Bullet list')}
      {btn('1.', () => editor.chain().focus().toggleOrderedList().run(), editor.isActive('orderedList'), 'Ordered list')}
      {btn('☐', () => editor.chain().focus().toggleTaskList().run(), editor.isActive('taskList'), 'Task list')}
      <span class="md-toolbar-sep" />
      {btn('"', () => editor.chain().focus().toggleBlockquote().run(), editor.isActive('blockquote'), 'Blockquote')}
      {btn('⌥⌥', () => editor.chain().focus().setHorizontalRule().run(), false, 'Horizontal rule')}
    </div>
  )
}

// ── Types ────────────────────────────────────────────────────────────────────

type SaveState = 'idle' | 'saving' | 'saved' | 'error'

export interface MarkdownEditorProps {
  projectSlug: string
  filePath: string
}

// ── Component ────────────────────────────────────────────────────────────────

export function MarkdownEditor({ projectSlug, filePath }: MarkdownEditorProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const editorRef = useRef<Editor | null>(null)
  const saveTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const latestContentRef = useRef<string>('')

  const isDirtyRef = useRef(false)
  const frontmatterRef = useRef<string | null>(null)
  const [frontmatterFields, setFrontmatterFields] = useState<FrontmatterField[]>([])

  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [saveState, setSaveState] = useState<SaveState>('idle')

  // Force re-render for toolbar active-state updates
  const [, forceUpdate] = useState(0)

  // Filename for display (last segment of path)
  const fileName = filePath.split('/').pop() ?? filePath

  // ── Save ──────────────────────────────────────────────────────────────────

  const doSave = useCallback(async () => {
    if (!editorRef.current) return
    try {
      setSaveState('saving')
      const md = editorRef.current.getMarkdown()
      const full = frontmatterRef.current ? frontmatterRef.current + '\n' + md : md
      await apiWriteFile(projectSlug, filePath, full)
      setSaveState('saved')
      // Revert to idle after 2 s
      setTimeout(() => setSaveState(s => s === 'saved' ? 'idle' : s), 2000)
    } catch (err) {
      console.error('[MarkdownEditor] save error', err)
      setSaveState('error')
      setTimeout(() => setSaveState(s => s === 'error' ? 'idle' : s), 4000)
    }
  }, [projectSlug, filePath])

  const scheduleSave = useCallback(() => {
    if (saveTimerRef.current !== null) clearTimeout(saveTimerRef.current)
    saveTimerRef.current = setTimeout(doSave, 1500)
  }, [doSave])

  // Flush pending save immediately (used by Cmd/Ctrl+S and on unmount)
  const flushSave = useCallback(() => {
    if (saveTimerRef.current !== null) {
      clearTimeout(saveTimerRef.current)
      saveTimerRef.current = null
    }
    return doSave()
  }, [doSave])

  // ── Keyboard shortcut: Cmd/Ctrl+S ────────────────────────────────────────

  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 's') {
        e.preventDefault()
        void flushSave()
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [flushSave])

  // ── Load content + init TipTap ────────────────────────────────────────────

  useEffect(() => {
    let destroyed = false

    const init = async () => {
      setLoading(true)
      setLoadError(null)
      setSaveState('idle')
      isDirtyRef.current = false
      frontmatterRef.current = null
      setFrontmatterFields([])

      let initialContent = ''
      try {
        initialContent = await apiReadFile(projectSlug, filePath)
      } catch (err) {
        if (!destroyed) {
          setLoadError(String(err))
          setLoading(false)
        }
        return
      }

      if (destroyed || !containerRef.current) return

      // Strip YAML frontmatter before passing to TipTap
      const { frontmatter, body } = parseFrontmatter(initialContent)
      frontmatterRef.current = frontmatter
      setFrontmatterFields(frontmatter !== null ? parseFrontmatterFields(frontmatter) : [])

      // Clear any previous editor DOM
      containerRef.current.innerHTML = ''

      latestContentRef.current = body

      let ed: Editor
      try {
        ed = new Editor({
          element: null, // deferred mount
          extensions: [
            StarterKit,
            Markdown,
            Table.configure({ resizable: false }),
            TableRow,
            TableCell,
            TableHeader,
            TaskList,
            TaskItem.configure({ nested: true }),
          ],
          content: body,
          editorProps: {
            attributes: { class: 'tiptap-editor' },
          },
        })
      } catch (err) {
        console.error('[MarkdownEditor] TipTap init error', err)
        if (!destroyed) {
          setLoadError(`Editor failed to initialise: ${err}`)
          setLoading(false)
        }
        return
      }

      // Wire up change listener (no spurious-event guard needed — on('update') only fires on edits)
      ed.on('update', () => {
        latestContentRef.current = ed.getMarkdown()
        isDirtyRef.current = true
        scheduleSave()
        forceUpdate(n => n + 1)
      })

      // Refresh toolbar active-states on cursor moves
      ed.on('selectionUpdate', () => forceUpdate(n => n + 1))

      ed.mount(containerRef.current!)

      if (destroyed) {
        ed.destroy()
        return
      }

      editorRef.current = ed
      setLoading(false)
    }

    void init()

    return () => {
      destroyed = true
      // Cancel debounced save — flush synchronously best-effort
      if (saveTimerRef.current !== null) {
        clearTimeout(saveTimerRef.current)
        saveTimerRef.current = null
      }
      // Flush final content only if the user actually made changes
      if (editorRef.current) {
        if (isDirtyRef.current) {
          const md = editorRef.current.getMarkdown()
          const full = frontmatterRef.current ? frontmatterRef.current + '\n' + md : md
          void apiWriteFile(projectSlug, filePath, full).catch(() => {/* best-effort */})
        }
        editorRef.current.destroy()
        editorRef.current = null
      }
    }
  // Re-run when the file changes (user clicks a different .md)
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectSlug, filePath])

  // ── Frontmatter change handler ────────────────────────────────────────────

  const handleFrontmatterChange = useCallback((fields: FrontmatterField[]) => {
    setFrontmatterFields(fields)
    frontmatterRef.current = fieldsToFrontmatter(fields)
    scheduleSave()
  }, [scheduleSave])

  // ── Save status label ─────────────────────────────────────────────────────

  const saveLabel =
    saveState === 'saving' ? 'Saving…'
    : saveState === 'saved' ? 'Saved'
    : saveState === 'error' ? 'Save failed'
    : null

  // ── Render ────────────────────────────────────────────────────────────────

  return (
    <div class="md-editor-panel">
      {/* Header */}
      <div class="main-header md-editor-header">
        <div class="main-header-left">
          <div class="main-header-title">{fileName}</div>
          <div class="main-header-meta">
            <span class="main-header-cwd">{filePath}</span>
          </div>
        </div>
        {saveLabel && (
          <div class={`md-editor-save-status ${saveState}`}>
            {saveLabel}
          </div>
        )}
      </div>

      {/* Toolbar — only shown when editor is ready */}
      {!loading && !loadError && <Toolbar editor={editorRef.current} />}

      {/* Body */}
      {loadError && (
        <div class="state-message">
          <div class="state-icon" style={{ color: 'var(--status-error)' }}>⚠</div>
          <div class="state-title">Failed to load file</div>
          <div class="state-subtitle">{loadError}</div>
        </div>
      )}
      {loading && !loadError && (
        <div class="state-message">
          <div class="state-subtitle">Loading…</div>
        </div>
      )}
      {/* Frontmatter key-value editor */}
      {frontmatterFields.length > 0 && (
        <FrontmatterEditor fields={frontmatterFields} onChange={handleFrontmatterChange} />
      )}
      {/* Always rendered so containerRef is mounted before useEffect runs */}
      <div class="md-editor-scroll" style={{ display: loading || loadError ? 'none' : undefined }}>
        <div class="md-editor-container" ref={containerRef} />
      </div>
    </div>
  )
}
