/**
 * Find-in-terminal bar, backed by @xterm/addon-search.
 *
 * Rendered by TerminalView inside the terminal shell (top-right, floating
 * over the grid like the resize pill). Opened via the `find` keybind
 * action (default secondary+F) or the session "⋮" menu — both flip the
 * `terminalFindOpen` signal in store.ts; this component only handles the
 * UI + addon calls while open.
 *
 * Search is incremental while typing (the current match expands as the
 * query grows instead of jumping ahead), Enter/Shift+Enter and the ‹ ›
 * buttons step through matches, Escape closes. Match highlighting uses
 * the addon's decoration support, which is also what makes
 * onDidChangeResults report the "n/total" counter.
 */

import type { SearchAddon } from '@xterm/addon-search'
import { useCallback, useEffect, useRef, useState } from 'preact/hooks'

/**
 * Decoration colors for match highlighting. The addon requires the two
 * overview-ruler colors whenever decorations are enabled. Colors follow
 * the browser convention: dim yellow for matches, orange for the active
 * one — chosen for contrast against the dark terminal background rather
 * than pulled from the theme (a themed light background would need a
 * bigger rework of these anyway).
 */
const SEARCH_DECORATIONS = {
  matchBackground: '#6b5900',
  matchOverviewRuler: '#6b5900',
  activeMatchBackground: '#c2410c',
  activeMatchColorOverviewRuler: '#c2410c',
}

const SEARCH_OPTIONS = { decorations: SEARCH_DECORATIONS }

export function TerminalFindBar({ addon, onClose }: {
  addon: SearchAddon
  onClose: () => void
}) {
  const inputRef = useRef<HTMLInputElement>(null)
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<{ index: number; count: number } | null>(null)

  // Focus the input on mount. On touch this deliberately pops the
  // on-screen keyboard — typing the query is the immediate next step.
  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  // Match counter. The event only fires while decorations are active,
  // which they always are here (SEARCH_OPTIONS). resultIndex is -1 when
  // nothing matched.
  useEffect(() => {
    const d = addon.onDidChangeResults(({ resultIndex, resultCount }) => {
      setResults({ index: resultIndex, count: resultCount })
    })
    return () => d.dispose()
  }, [addon])

  // Clear highlights when the bar unmounts (close, session switch).
  useEffect(() => () => addon.clearDecorations(), [addon])

  const search = useCallback((q: string) => {
    setQuery(q)
    if (!q) {
      addon.clearDecorations()
      setResults(null)
      return
    }
    // incremental: keep (and extend) the current match while typing
    // instead of jumping to the next occurrence on every keystroke.
    addon.findNext(q, { ...SEARCH_OPTIONS, incremental: true })
  }, [addon])

  const next = useCallback(() => { if (query) addon.findNext(query, SEARCH_OPTIONS) }, [addon, query])
  const prev = useCallback(() => { if (query) addon.findPrevious(query, SEARCH_OPTIONS) }, [addon, query])

  const onKeyDown = useCallback((ev: KeyboardEvent) => {
    if (ev.key === 'Escape') {
      ev.preventDefault()
      onClose()
      return
    }
    if (ev.key === 'Enter') {
      ev.preventDefault()
      if (ev.shiftKey) prev()
      else next()
      return
    }
    // Secondary+F while already focused here: keep the browser's native
    // find suppressed (the terminal keybind can't intercept it because
    // the event never reaches xterm's textarea) and just reselect.
    if (ev.key.toLowerCase() === 'f' && (ev.metaKey || ev.ctrlKey)) {
      ev.preventDefault()
      inputRef.current?.select()
    }
  }, [next, prev, onClose])

  const hasMatches = results != null && results.count > 0

  return (
    <div class="terminal-find-bar" role="search" aria-label="Find in terminal">
      <input
        ref={inputRef}
        class="terminal-find-input"
        type="text"
        // Search queries are terminal text verbatim; phone keyboards must
        // not "help".
        autocomplete="off"
        autocapitalize="off"
        autocorrect="off"
        spellcheck={false}
        enterkeyhint="search"
        placeholder="Find"
        value={query}
        onInput={(ev) => search((ev.target as HTMLInputElement).value)}
        onKeyDown={onKeyDown}
      />
      <span class={`terminal-find-count${query && !hasMatches ? ' no-matches' : ''}`}>
        {query ? (hasMatches ? `${results.index + 1}/${results.count}` : '0/0') : ''}
      </span>
      <button
        type="button"
        class="terminal-find-btn"
        onClick={prev}
        disabled={!query}
        title="Previous match (Shift+Enter)"
        aria-label="Previous match"
      >‹</button>
      <button
        type="button"
        class="terminal-find-btn"
        onClick={next}
        disabled={!query}
        title="Next match (Enter)"
        aria-label="Next match"
      >›</button>
      <button
        type="button"
        class="terminal-find-btn"
        onClick={onClose}
        title="Close (Escape)"
        aria-label="Close find bar"
      >✕</button>
    </div>
  )
}
