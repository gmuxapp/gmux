/**
 * MarkdownEditor — CodeMirror 6 hybrid markdown editor for .md files.
 *
 * Opened when the user clicks a .md file in the file tree.
 * Reads content via GET /v1/fs/{slug}/read, writes back via POST /v1/fs/{slug}/write.
 * Auto-saves 1.5 s after the last edit, with Cmd/Ctrl+S as an explicit trigger.
 *
 * Hybrid UX (Obsidian-style via codemirror-live-markdown):
 *   - Headings render at visual size, bold/italic syntax hidden on unfocused lines
 *   - GFM tables render as HTML tables
 *   - Images render as inline previews
 *   - Raw markdown shown on the active line
 */

import { useEffect, useRef, useCallback, useState } from 'preact/hooks'
import '@fontsource/lora/400.css'
import '@fontsource/lora/700.css'
import { EditorState } from '@codemirror/state'
import { EditorView, keymap } from '@codemirror/view'
import { defaultKeymap, historyKeymap, history, indentWithTab } from '@codemirror/commands'
import { markdown } from '@codemirror/lang-markdown'
import { Table } from '@lezer/markdown'
import {
  livePreviewPlugin,
  markdownStylePlugin,
  editorTheme,
  tableField,
  imageField,
  collapseOnSelectionFacet,
  mouseSelectingField,
  setMouseSelecting,
} from 'codemirror-live-markdown'

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

/** Parse YAML frontmatter string (including --- delimiters) into key-value pairs. */
function parseFrontmatterFields(raw: string): FrontmatterField[] {
  const inner = raw.slice(4) // strip leading '---\n'
  const end = inner.lastIndexOf('---')
  const body = end >= 0 ? inner.slice(0, end) : inner
  const fields: FrontmatterField[] = []
  for (const line of body.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) continue
    const colonIdx = trimmed.indexOf(':')
    if (colonIdx < 0) continue
    fields.push({ key: trimmed.slice(0, colonIdx).trim(), value: trimmed.slice(colonIdx + 1).trim() })
  }
  return fields
}

/** Reconstruct frontmatter string from key-value pairs. Returns null when empty. */
function fieldsToFrontmatter(fields: FrontmatterField[]): string | null {
  if (fields.length === 0) return null
  return '---\n' + fields.map(({ key, value }) => `${key}: ${value}`).join('\n') + '\n---'
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

// ── Theme (defined outside component to avoid re-creation on render) ─────────

const gmuxTheme = EditorView.theme({
  '&': {
    fontFamily: "'Lora', Georgia, serif",
    fontSize: '15px',
    height: '100%',
  },
  '.cm-scroller': {
    fontFamily: "'Lora', Georgia, serif",
    lineHeight: '1.75',
    overflowY: 'auto',
  },
  '.cm-content': {
    padding: '32px 24px',
    maxWidth: '720px',
    margin: '0 auto',
    caretColor: 'var(--accent, oklch(65% 0.18 250))',
  },
  '.cm-line': { padding: '0' },
  '.cm-focused': { outline: 'none' },
  '&.cm-focused': { outline: 'none' },
  '.cm-cursor': { borderLeftColor: 'var(--accent, oklch(65% 0.18 250))' },
  '.cm-selectionBackground': {
    backgroundColor: 'var(--selection-bg, rgba(99,179,237,0.2))',
  },
  '&.cm-focused .cm-selectionBackground': {
    backgroundColor: 'var(--selection-bg, rgba(99,179,237,0.25))',
  },
  '.cm-gutters': { display: 'none' },
  // Heading sizes (markdownStylePlugin applies cm-heading-N classes)
  '.cm-heading-1': { fontSize: '1.8em', fontWeight: '700', fontFamily: "'Instrument Sans', sans-serif", lineHeight: '1.3' },
  '.cm-heading-2': { fontSize: '1.4em', fontWeight: '600', fontFamily: "'Instrument Sans', sans-serif", lineHeight: '1.3' },
  '.cm-heading-3': { fontSize: '1.15em', fontWeight: '600', fontFamily: "'Instrument Sans', sans-serif", lineHeight: '1.3' },
  // Inline code
  '.cm-inline-code': {
    background: 'var(--bg-selected)',
    borderRadius: '3px',
    fontFamily: "'JetBrains Mono', 'Fira Code', monospace",
    fontSize: '0.88em',
    padding: '1px 5px',
  },
  // Block code
  '.cm-code-block': {
    background: 'var(--bg-selected)',
    borderRadius: 'var(--radius)',
    fontFamily: "'JetBrains Mono', 'Fira Code', monospace",
    fontSize: '0.88em',
    lineHeight: '1.55',
    padding: '14px 18px',
    display: 'block',
  },
  // Blockquote
  '.cm-blockquote': {
    borderLeft: '3px solid var(--border, oklch(35% 0 0))',
    color: 'var(--text-muted)',
    paddingLeft: '16px',
  },
  // Links
  '.cm-link': { color: 'var(--accent, oklch(65% 0.18 250))' },
  // Tables (rendered by tableField)
  '.cm-table-wrapper table': { borderCollapse: 'collapse', width: '100%', margin: '0.8em 0', fontSize: '14px' },
  '.cm-table-wrapper th': {
    background: 'var(--bg-selected)', fontFamily: "'Instrument Sans', sans-serif",
    fontWeight: '600', fontSize: '0.9em', padding: '6px 10px', textAlign: 'left',
    border: '1px solid var(--border)',
  },
  '.cm-table-wrapper td': { padding: '6px 10px', border: '1px solid var(--border)' },
}, { dark: true })

// ── Types ────────────────────────────────────────────────────────────────────

type SaveState = 'idle' | 'saving' | 'saved' | 'error'

export interface MarkdownEditorProps {
  projectSlug: string
  filePath: string
}

// ── Component ────────────────────────────────────────────────────────────────

export function MarkdownEditor({ projectSlug, filePath }: MarkdownEditorProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const viewRef = useRef<EditorView | null>(null)
  const saveTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const isDirtyRef = useRef(false)
  const frontmatterRef = useRef<string | null>(null)
  const [frontmatterFields, setFrontmatterFields] = useState<FrontmatterField[]>([])

  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [saveState, setSaveState] = useState<SaveState>('idle')

  const fileName = filePath.split('/').pop() ?? filePath

  // ── Save ──────────────────────────────────────────────────────────────────

  const doSave = useCallback(async () => {
    if (!viewRef.current) return
    try {
      setSaveState('saving')
      const md = viewRef.current.state.doc.toString()
      const full = frontmatterRef.current ? frontmatterRef.current + '\n' + md : md
      await apiWriteFile(projectSlug, filePath, full)
      setSaveState('saved')
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

  // ── Load content + init CodeMirror ────────────────────────────────────────

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

      const { frontmatter, body } = parseFrontmatter(initialContent)
      frontmatterRef.current = frontmatter
      setFrontmatterFields(frontmatter !== null ? parseFrontmatterFields(frontmatter) : [])

      containerRef.current.innerHTML = ''

      const state = EditorState.create({
        doc: body,
        extensions: [
          history(),
          markdown({ extensions: [Table] }),   // Table parser required for tableField
          collapseOnSelectionFacet.of(true),   // enable live preview collapsing
          mouseSelectingField,                 // track mouse selection state
          livePreviewPlugin,                   // hide markers on unfocused lines
          markdownStylePlugin,                 // heading sizes, bold/italic styles
          tableField,                          // GFM tables → HTML
          imageField(),                        // inline image previews
          editorTheme,                         // package default animations
          // App theme override
          gmuxTheme,
          EditorView.lineWrapping,
          keymap.of([...defaultKeymap, ...historyKeymap, indentWithTab]),
          EditorView.updateListener.of(update => {
            if (update.docChanged) {
              isDirtyRef.current = true
              scheduleSave()
            }
          }),
        ],
      })

      const view = new EditorView({
        state,
        parent: containerRef.current,
      })

      // Required for livePreviewPlugin to hide markers on unfocused lines
      view.contentDOM.addEventListener('mousedown', () => {
        view.dispatch({ effects: setMouseSelecting.of(true) })
      })
      document.addEventListener('mouseup', () => {
        requestAnimationFrame(() => {
          view.dispatch({ effects: setMouseSelecting.of(false) })
        })
      })

      if (destroyed) {
        view.destroy()
        return
      }

      viewRef.current = view
      setLoading(false)
    }

    void init()

    return () => {
      destroyed = true
      if (saveTimerRef.current !== null) {
        clearTimeout(saveTimerRef.current)
        saveTimerRef.current = null
      }
      if (viewRef.current) {
        if (isDirtyRef.current) {
          const md = viewRef.current.state.doc.toString()
          const full = frontmatterRef.current ? frontmatterRef.current + '\n' + md : md
          void apiWriteFile(projectSlug, filePath, full).catch(() => {/* best-effort */})
        }
        viewRef.current.destroy()
        viewRef.current = null
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

      {frontmatterFields.length > 0 && (
        <FrontmatterEditor fields={frontmatterFields} onChange={handleFrontmatterChange} />
      )}

      {/* Always rendered so containerRef is mounted before useEffect runs */}
      <div
        class="md-editor-scroll"
        style={{ display: loading || loadError ? 'none' : undefined }}
      >
        <div class="md-editor-container" ref={containerRef} />
      </div>
    </div>
  )
}
