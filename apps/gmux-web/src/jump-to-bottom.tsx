import { useEffect, useState } from 'preact/hooks'
import type { Terminal } from '@xterm/xterm'

/**
 * Floating "jump to bottom" button that overlays the bottom-right of an
 * xterm viewport when the user has scrolled up. Hidden when the viewport
 * is already at the bottom.
 *
 * Designed for any view that mounts an xterm.js Terminal: pass the
 * Terminal instance once it's been created. Tracks scroll position via
 * `term.onScroll`, which fires for both user-initiated and programmatic
 * scrolls, so the visibility stays in sync without polling.
 *
 * The button is positioned absolutely; the parent must establish a
 * positioning context (`.terminal-shell` already does, via
 * `position: relative`).
 */
export function JumpToBottom({ term }: { term: Terminal | null }) {
  const [atBottom, setAtBottom] = useState(true)

  useEffect(() => {
    if (!term) return
    const update = () => {
      const buf = term.buffer.active
      // viewportY === baseY means the viewport's top row is the same as
      // the buffer's bottom-of-scrollback baseline, i.e. nothing is
      // scrolled off the bottom.
      setAtBottom(buf.viewportY >= buf.baseY)
    }
    update()
    const sub = term.onScroll(update)
    return () => sub.dispose()
  }, [term])

  if (atBottom || !term) return null
  return (
    <button
      type="button"
      class="jump-to-bottom"
      aria-label="Jump to bottom"
      title="Jump to bottom"
      onClick={() => term.scrollToBottom()}
    >
      ↓
    </button>
  )
}
