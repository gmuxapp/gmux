import { useEffect, useState } from 'preact/hooks'
import type { WTerm } from '@wterm/dom'

/**
 * Floating "jump to bottom" button that overlays the bottom-right of a
 * wterm viewport when the user has scrolled up.
 *
 * Tracks scroll position via a DOM scroll event on term.element.
 */
export function JumpToBottom({ term }: { term: WTerm | null }) {
  const [atBottom, setAtBottom] = useState(true)

  useEffect(() => {
    if (!term) return
    const el = term.element
    const update = () => {
      setAtBottom(el.scrollHeight - el.scrollTop - el.clientHeight < 5)
    }
    update()
    el.addEventListener('scroll', update, { passive: true })
    return () => el.removeEventListener('scroll', update)
  }, [term])

  if (atBottom || !term) return null
  return (
    <button
      type="button"
      class="jump-to-bottom"
      aria-label="Jump to bottom"
      title="Jump to bottom"
      onClick={() => {
        const el = term.element
        el.scrollTop = el.scrollHeight
      }}
    >
      ↓
    </button>
  )
}
