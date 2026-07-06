/**
 * Long-press text sheet: the whole terminal buffer rendered as plain,
 * natively-selectable text, scrolled to the row the user pressed, with
 * the clipboard actions pinned at the bottom.
 *
 * The long-press is a "give me my clipboard actions" gesture, so Copy all
 * and Paste are co-equal peers (neither is the accent), with a quiet Close
 * set apart. Copy closes the sheet immediately and a toast confirms —
 * confirmation is decoupled from the surface, so no lingering sheet and
 * no arbitrary auto-close timer.
 *
 * Why plain text instead of growing xterm's own selection: on touch,
 * native selection (OS handles, magnifier, the system Copy callout) is
 * far better than anything we can build over the terminal's canvas — and
 * text copied this way is same-origin clipboard content, which WebKit
 * exempts from the iOS paste-confirmation prompt. So Copy and a
 * promptless Paste both fall out of this one surface.
 *
 * Presentational: the caller snapshots the rows and the pressed row at
 * press time (see readTerminalText / pressedBufferRow), so terminal
 * output scrolling underneath can't make the sheet lie.
 *
 * Portaling, keyboard-aware sizing, click dismiss, and button styling
 * all live in the shared {@link SheetBackdrop} / {@link SheetButton} — see
 * there for the touch-isolation and iOS reasoning.
 */
import { useLayoutEffect, useRef } from 'preact/hooks'
import { CopyButton, SheetBackdrop, SheetButton } from './sheet'

export interface TerminalTextSheetProps {
  /** Buffer rows as plain text, one per visual row. */
  lines: string[]
  /** Index into `lines` to centre on (the pressed row). */
  anchorRow: number
  onPaste: () => void
  onClose: () => void
}

export function TerminalTextSheet({ lines, anchorRow, onPaste, onClose }: TerminalTextSheetProps) {
  const bodyRef = useRef<HTMLDivElement>(null)
  const anchorRef = useRef<HTMLSpanElement>(null)

  // Centre the pressed row so the user lands where their finger was.
  useLayoutEffect(() => {
    const body = bodyRef.current
    const anchor = anchorRef.current
    if (!body || !anchor) return
    body.scrollTop = anchor.offsetTop - body.clientHeight / 2 + anchor.offsetHeight / 2
  }, [])

  const before = lines.slice(0, anchorRow)
  const after = lines.slice(anchorRow + 1)

  return (
    <SheetBackdrop onClose={onClose}>
      <div class="modal-panel text-sheet" role="dialog" aria-label="Terminal text">
        {/* Single text flow (only the anchor line is wrapped, as the
            scroll target) so native selection runs clean. */}
        <div class="text-sheet-body" ref={bodyRef}>
          <pre class="text-sheet-pre">
            {before.length > 0 && `${before.join('\n')}\n`}
            <span ref={anchorRef}>{lines[anchorRow] ?? ''}</span>
            {after.length > 0 && `\n${after.join('\n')}`}
          </pre>
        </div>
        <div class="text-sheet-footer">
          <SheetButton quiet onActivate={onClose}>Close</SheetButton>
          <CopyButton label="Copy all" text={lines.join('\n')} onClose={onClose} />
          <SheetButton onActivate={() => { onPaste(); onClose() }}>Paste</SheetButton>
        </div>
      </div>
    </SheetBackdrop>
  )
}
