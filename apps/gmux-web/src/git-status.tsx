/**
 * GitStatus — inline git change indicator for the file tree header.
 *
 * Polls GET /v1/git/{slug}/status every 10 s and renders a compact
 * badge showing changed files, insertions, and deletions.
 * Clicking it launches a new terminal pane running:
 *   git diff HEAD | diff-so-fancy | less --tabs=4 -RFX
 *
 * Renders nothing when there are no changes.
 */

import { useState, useEffect, useCallback } from 'preact/hooks'
import { launchCommand } from './store'

// ── Types ──

export interface GitStatusResult {
  files: number
  insertions: number
  deletions: number
}

interface FormattedGitStat {
  files: string
  insertions: string | null
  deletions: string | null
}

// ── Pure helpers (exported for tests) ──

/** Format a GitStatusResult into display strings for each part. */
export function formatGitStat(r: GitStatusResult): FormattedGitStat {
  return {
    files: `${r.files}~`,
    insertions: r.insertions > 0 ? `+${r.insertions}` : null,
    deletions: r.deletions > 0 ? `\u2212${r.deletions}` : null,
  }
}

// ── Component ──

export function GitStatus({
  projectSlug,
  cwd,
}: {
  projectSlug: string
  cwd: string
}) {
  const [status, setStatus] = useState<GitStatusResult | null>(null)

  const poll = useCallback(async () => {
    try {
      const resp = await fetch(`/v1/git/${encodeURIComponent(projectSlug)}/status`)
      if (!resp.ok) return
      const json = await resp.json()
      if (json.ok && json.data) {
        setStatus(json.data as GitStatusResult)
      }
    } catch {
      // network error — silently ignore, keep last known state
    }
  }, [projectSlug])

  useEffect(() => {
    void poll()
    const id = setInterval(() => void poll(), 10_000)
    return () => clearInterval(id)
  }, [poll])

  if (!status || status.files === 0) return null

  const fmt = formatGitStat(status)

  const handleClick = (e: MouseEvent) => {
    e.stopPropagation()
    void launchCommand(
      ['sh', '-c', 'git diff HEAD | diff-so-fancy | less --tabs=4 -RFX'],
      { cwd },
    )
  }

  return (
    <button
      class="git-status-badge"
      onClick={handleClick}
      title="Open diff (requires diff-so-fancy)"
    >
      <span class="git-status-files">{fmt.files}</span>
      {fmt.insertions && <span class="git-status-ins">{fmt.insertions}</span>}
      {fmt.deletions && <span class="git-status-del">{fmt.deletions}</span>}
    </button>
  )
}
