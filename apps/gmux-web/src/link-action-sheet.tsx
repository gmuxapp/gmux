/**
 * Action sheet for long-pressing a link in the terminal.
 *
 * Centered modal (via the shared {@link SheetBackdrop} — portaled,
 * keyboard-aware, click-dismissed) rather than anchored to the touch
 * point: consistent placement, no clamp/flip math, and the link itself
 * stays visible. Shows the resolved target URI — for OSC 8 hyperlinks the
 * buffer only paints a label, so this is the one place a user can inspect
 * the real target before opening it.
 *
 * The sheet holds a snapshot ({@link LinkInfo}) taken at press time;
 * it never reads live buffer state, so terminal output scrolling the
 * link away can't make it lie, and there's no reason to auto-dismiss.
 */
import type { LinkInfo } from './terminal-link'
import { CopyButton, SheetBackdrop, SheetButton } from './sheet'

export interface LinkActionSheetProps {
  link: LinkInfo
  onClose: () => void
}

export function LinkActionSheet({ link, onClose }: LinkActionSheetProps) {
  const showLabel = link.label !== link.uri

  const handleOpen = () => {
    window.open(link.uri, '_blank', 'noopener,noreferrer')
    onClose()
  }

  return (
    <SheetBackdrop onClose={onClose}>
      <div class="modal-panel link-sheet" role="dialog" aria-label="Link actions">
        <div class="link-sheet-preview">
          {showLabel && <div class="link-sheet-label">{link.label}</div>}
          <div class="link-sheet-uri">{link.uri}</div>
        </div>
        <div class="link-sheet-actions">
          <CopyButton label="Copy URL" text={link.uri} />
          <SheetButton primary onActivate={handleOpen}>Open</SheetButton>
        </div>
      </div>
    </SheetBackdrop>
  )
}
