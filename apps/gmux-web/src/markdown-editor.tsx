/**
 * MarkdownEditor — in-browser Milkdown WYSIWYG editor for .md files.
 *
 * Opened when the user clicks a .md file in the file tree.
 * Reads content via GET /v1/fs/{slug}/read, writes back via POST /v1/fs/{slug}/write.
 * Auto-saves 1.5 s after the last edit, with Cmd/Ctrl+S as an explicit trigger.
 */

import { useEffect, useRef, useCallback, useState } from 'preact/hooks'
import { Editor, rootCtx, defaultValueCtx } from '@milkdown/kit/core'
import { commonmark } from '@milkdown/kit/preset/commonmark'
import { gfm } from '@milkdown/kit/preset/gfm'
import { history } from '@milkdown/kit/plugin/history'
import { clipboard } from '@milkdown/kit/plugin/clipboard'
import { listener, listenerCtx } from '@milkdown/kit/plugin/listener'
import { getMarkdown } from '@milkdown/kit/utils'

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

  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [saveState, setSaveState] = useState<SaveState>('idle')

  // Filename for display (last segment of path)
  const fileName = filePath.split('/').pop() ?? filePath

  // ── Save ──────────────────────────────────────────────────────────────────

  const doSave = useCallback(async () => {
    if (!editorRef.current) return
    try {
      setSaveState('saving')
      const md = editorRef.current.action(getMarkdown())
      await apiWriteFile(projectSlug, filePath, md)
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

  // ── Load content + init Milkdown ──────────────────────────────────────────

  useEffect(() => {
    let destroyed = false

    const init = async () => {
      setLoading(true)
      setLoadError(null)
      setSaveState('idle')

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

      // Clear any previous editor DOM
      containerRef.current.innerHTML = ''

      latestContentRef.current = initialContent

      let ed: Editor
      try {
        ed = await Editor.make()
          .config((ctx: any) => {
            ctx.set(rootCtx, containerRef.current!)
            ctx.set(defaultValueCtx, initialContent)
          })
          .use(commonmark)
          .use(gfm)
          .use(history)
          .use(clipboard)
          .use(listener)
          .config((ctx: any) => {
            ctx.get(listenerCtx).markdownUpdated((_ctx: any, markdown: string) => {
              latestContentRef.current = markdown
              scheduleSave()
            })
          })
          .create()
      } catch (err) {
        console.error('[MarkdownEditor] Milkdown init error', err)
        if (!destroyed) {
          setLoadError(`Editor failed to initialise: ${err}`)
          setLoading(false)
        }
        return
      }

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
      // Flush final content if editor was alive
      if (editorRef.current) {
        const md = editorRef.current.action(getMarkdown())
        void apiWriteFile(projectSlug, filePath, md).catch(() => {/* best-effort */})
        editorRef.current.destroy()
        editorRef.current = null
      }
    }
  // Re-run when the file changes (user clicks a different .md)
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectSlug, filePath])

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
      {/* Always rendered so containerRef is mounted before useEffect runs */}
      <div class="md-editor-scroll" style={{ display: loading || loadError ? 'none' : undefined }}>
        <div class="md-editor-container" ref={containerRef} />
      </div>
    </div>
  )
}
