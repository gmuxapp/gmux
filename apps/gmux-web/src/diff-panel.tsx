/**
 * DiffPanel — in-browser unified diff viewer for a project cwd.
 *
 * Fetches GET /v1/git/{slug}/diff?cwd=<path>, parses the patch with
 * @pierre/diffs's `parsePatchFiles`, and renders each file diff using
 * the React `FileDiff` component from @pierre/diffs/react (aliased to
 * preact/compat via @preact/preset-vite).
 */

import { useEffect, useState, useCallback } from 'preact/hooks'
import { closeDiffView } from './store'
import { parsePatchFiles } from '@pierre/diffs'
import { FileDiff } from '@pierre/diffs/react'
import type { FileDiffMetadata, FileDiffOptions } from '@pierre/diffs'

// ── Types ──

interface DiffPanelProps {
  projectSlug: string
  cwd: string
}

// ── Diff options (stable reference — defined outside component) ──

const DIFF_BASE_OPTIONS: FileDiffOptions<undefined> = {
  theme: 'dark-plus',
  diffStyle: 'split',
  overflow: 'wrap',
  hunkSeparators: 'line-info',
}
// ── Component ──

export function DiffPanel({ projectSlug, cwd }: DiffPanelProps) {
  const [files, setFiles] = useState<FileDiffMetadata[] | null>(null)
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())

  const toggleCollapse = useCallback((key: string) => {
    setCollapsed(prev => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }, [])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  // Fetch the diff from the backend.
  useEffect(() => {
    setLoading(true)
    setError(null)
    setFiles(null)
    const url = `/v1/git/${encodeURIComponent(projectSlug)}/diff?cwd=${encodeURIComponent(cwd)}`
    fetch(url)
      .then(async res => {
        if (!res.ok) throw new Error(`${res.status} ${res.statusText}`)
        return res.text()
      })
      .then(text => {
        if (text.trim() === '') {
          setFiles([])
          setCollapsed(new Set())
        } else {
          const patches = parsePatchFiles(text, 'gmux-diff')
          const allFiles = patches.flatMap(p => p.files)
          setFiles(allFiles)
          setCollapsed(new Set(allFiles.map((f, i) => f.name ?? f.prevName ?? String(i))))
        }
        setLoading(false)
      })
      .catch(err => {
        setError(String(err))
        setLoading(false)
      })
  }, [projectSlug, cwd])

  const shortCwd = cwd.replace(/^\/Users\/[^/]+/, '~').replace(/^\/home\/[^/]+/, '~')

  return (
    <div class="diff-panel">
      <div class="diff-panel-header">
        <div class="diff-panel-title">
          <span class="diff-panel-label">git diff HEAD</span>
          <span class="diff-panel-cwd">{shortCwd}</span>
        </div>
        <button
          class="diff-panel-close"
          onClick={() => closeDiffView(projectSlug)}
          title="Close diff view"
        >
          ✕
        </button>
      </div>

      <div class="diff-panel-body">
        {loading && (
          <div class="diff-panel-state">Loading diff…</div>
        )}
        {error && (
          <div class="diff-panel-state diff-panel-error">Error: {error}</div>
        )}
        {!loading && !error && files?.length === 0 && (
          <div class="diff-panel-state">No changes (working tree is clean).</div>
        )}
        {!loading && !error && files && files.length > 0 && (
          <div class="diff-files">
            {files.map((fileDiff, idx) => {
              const key = fileDiff.name ?? fileDiff.prevName ?? String(idx)
              const isCollapsed = collapsed.has(key)
              const options: FileDiffOptions<undefined> = { ...DIFF_BASE_OPTIONS, collapsed: isCollapsed }
              return (
                <div class="diff-file-wrapper" key={key}>
                  <button
                    class="diff-file-collapse-btn"
                    onClick={() => toggleCollapse(key)}
                    title={isCollapsed ? 'Expand' : 'Collapse'}
                  >
                    <span class="diff-file-collapse-chevron">{isCollapsed ? '▶' : '▼'}</span>
                    <span class="diff-file-collapse-name">{key}</span>
                  </button>
                  <FileDiff fileDiff={fileDiff} options={options} />
                </div>
              )
            })}
          </div>
        )}
      </div>
    </div>
  )
}
