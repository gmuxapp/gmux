/**
 * DiffPanel — in-browser unified diff viewer for a project cwd.
 *
 * Fetches GET /v1/git/{slug}/diff?cwd=<path>, parses the patch with
 * @pierre/diffs's `parsePatchFiles`, and renders each file diff using
 * the vanilla JS `FileDiff` class (avoids Preact compat risk).
 */

import { useEffect, useRef, useState } from 'preact/hooks'
import { closeDiffView } from './store'
import { parsePatchFiles, FileDiff } from '@pierre/diffs'
import type { FileDiffOptions } from '@pierre/diffs'

// ── Types ──

interface DiffPanelProps {
  projectSlug: string
  cwd: string
}

// ── Component ──

export function DiffPanel({ projectSlug, cwd }: DiffPanelProps) {
  const [patch, setPatch] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const containerRef = useRef<HTMLDivElement>(null)
  // Track mounted FileDiff instances for cleanup.
  const instancesRef = useRef<FileDiff[]>([])

  // Fetch the diff from the backend.
  useEffect(() => {
    setLoading(true)
    setError(null)
    const url = `/v1/git/${encodeURIComponent(projectSlug)}/diff?cwd=${encodeURIComponent(cwd)}`
    fetch(url)
      .then(async res => {
        if (!res.ok) throw new Error(`${res.status} ${res.statusText}`)
        return res.text()
      })
      .then(text => {
        setPatch(text)
        setLoading(false)
      })
      .catch(err => {
        setError(String(err))
        setLoading(false)
      })
  }, [projectSlug, cwd])

  // Render FileDiff instances into the container once patch is available.
  useEffect(() => {
    if (!containerRef.current || patch === null) return

    // Destroy previous instances.
    for (const inst of instancesRef.current) {
      const el = (inst as any)._fileContainer as HTMLElement | undefined
      el?.remove()
    }
    instancesRef.current = []
    containerRef.current.innerHTML = ''

    if (patch.trim() === '') return // no changes

    const patches = parsePatchFiles(patch, 'gmux-diff')
    const files = patches.flatMap(p => p.files)

    for (const fileDiff of files) {
      const fileContainer = document.createElement('div')
      fileContainer.className = 'diff-file-container'
      containerRef.current.appendChild(fileContainer)

      const options: FileDiffOptions<undefined> = {
        theme: 'dark-plus',
        hunkSeparators: 'line-info',
      }
      const inst = new FileDiff(options)
      ;(inst as any)._fileContainer = fileContainer
      instancesRef.current.push(inst)
      inst.render({ fileDiff, fileContainer })
    }

    return () => {
      for (const inst of instancesRef.current) {
        const el = (inst as any)._fileContainer as HTMLElement | undefined
        el?.remove()
      }
      instancesRef.current = []
    }
  }, [patch])

  const shortCwd = cwd.replace(/^\/home\/[^/]+/, '~')

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
        {!loading && !error && patch?.trim() === '' && (
          <div class="diff-panel-state">No changes (working tree is clean).</div>
        )}
        {!loading && !error && patch && patch.trim() !== '' && (
          <div class="diff-files" ref={containerRef} />
        )}
      </div>
    </div>
  )
}
