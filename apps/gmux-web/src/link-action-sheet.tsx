/**
 * Action sheet for long-pressing a link in the terminal.
 *
 * Centered modal (reuses the settings-modal backdrop conventions)
 * rather than anchored to the touch point: consistent placement, no
 * clamp/flip math, and the link itself stays visible. Shows the
 * resolved target URI — for OSC 8 hyperlinks the buffer only paints a
 * label, so this is the one place a user can inspect the real target
 * before opening it.
 *
 * The sheet holds a snapshot ({@link LinkInfo}) taken at press time;
 * it never reads live buffer state, so terminal output scrolling the
 * link away can't make it lie, and there's no reason to auto-dismiss.
 */
import { useEffect, useRef, useState } from 'preact/hooks'
import type { LinkInfo } from './terminal-link'

export interface LinkActionSheetProps {
  link: LinkInfo
  onClose: () => void
}

type CopyState = 'idle' | 'copied' | 'failed'

const COPY_CLOSE_DELAY_MS = 600

export function LinkActionSheet({ link, onClose }: LinkActionSheetProps) {
  const [copyState, setCopyState] = useState<CopyState>('idle')
  const closeTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const showLabel = link.label !== link.uri

  // Don't let the post-copy auto-close fire after the sheet is already
  // gone (e.g. dismissed via the backdrop within the delay window).
  useEffect(() => () => {
    if (closeTimer.current !== null) clearTimeout(closeTimer.current)
  }, [])

  const handleCopy = () => {
    if (!navigator.clipboard) {
      setCopyState('failed')
      return
    }
    navigator.clipboard.writeText(link.uri).then(
      () => {
        setCopyState('copied')
        closeTimer.current = setTimeout(onClose, COPY_CLOSE_DELAY_MS)
      },
      () => setCopyState('failed'),
    )
  }

  const handleOpen = () => {
    window.open(link.uri, '_blank', 'noopener,noreferrer')
    onClose()
  }

  return (
    // Activate on pointerup, not click. The sheet opens mid-touch (the
    // 500ms hold fires while the finger is down on the terminal), so a
    // quick tap right after the long-press release gets coalesced with
    // it by iOS's gesture recognizer, which suppresses the synthesized
    // click — on whatever element it lands, regardless of touch-action
    // (that's decided by the element the gesture *started* on, the
    // terminal). pointerup fires on release reliably and bypasses click
    // synthesis entirely, while still being a release gesture (not
    // press-to-activate). The recognizer is touch-gated, so the sheet
    // only exists on touch devices; no keyboard-activation path to lose.
    <div
      class="modal-backdrop link-sheet-backdrop"
      onPointerUp={ev => {
        ev.stopPropagation()
        if (ev.target === ev.currentTarget) onClose()
      }}
    >
      <div class="modal-panel link-sheet" role="menu" aria-label="Link actions">
        <div class="link-sheet-preview">
          {showLabel && <div class="link-sheet-label">{link.label}</div>}
          <div class="link-sheet-uri">{link.uri}</div>
        </div>
        <button
          type="button"
          class="link-sheet-action"
          role="menuitem"
          onPointerUp={ev => { ev.stopPropagation(); handleCopy() }}
        >
          {copyState === 'idle' ? 'Copy URL' : copyState === 'copied' ? 'Copied ✓' : 'Copy failed'}
        </button>
        <button
          type="button"
          class="link-sheet-action"
          role="menuitem"
          onPointerUp={ev => { ev.stopPropagation(); handleOpen() }}
        >
          Open
        </button>
      </div>
    </div>
  )
}
