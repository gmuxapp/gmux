/**
 * Shared primitives for the terminal long-press sheets (link + text):
 * {@link SheetBackdrop} and {@link SheetButton}. One place for the portal,
 * keyboard, dismissal, and button styling all the sheets depend on.
 *
 * SheetBackdrop does three things every long-press sheet needs:
 *
 * 1. Portaled to <body>. Rendered in place, a sheet is a descendant of
 *    `.terminal-shell`, whose capture-phase touch handlers (pan + the
 *    long-press recognizer) then intercept every touch on it — a
 *    press-hold to select text re-fires long-press instead, and native
 *    selection never receives the touch. The portal moves the sheet out
 *    of that subtree so touches reach its content directly, and frees it
 *    from the shell's stacking context so it paints above the header and
 *    toolbar.
 *
 * 2. Constrained to the visible viewport via `--app-height` (the CSS lives
 *    on `.action-sheet-backdrop`). With the soft keyboard open the layout
 *    viewport still spans the full window behind the keyboard, so a plain
 *    inset:0 backdrop would centre the panel too low (partly occluded).
 *
 * 3. Dismiss on click, deferred a macrotask. click fires *after* the
 *    dismissing touch ends, so unmounting the sheet there can't tear down
 *    the terminal's iOS momentum-scroll layer mid-gesture. That mid-gesture
 *    teardown is exactly what killed inertial scrolling — the terminal
 *    coasts on a GPU layer that iOS drops if the DOM is torn down while the
 *    touch is live — when these dismissed on pointerup (pointerup fires
 *    while the touch is still in flight). We briefly used pointerup to
 *    catch a "quick tap right after the long-press" whose click got
 *    swallowed — but that swallowing turned out to be Orion-on-iOS
 *    specific; Safari/WebKit fire the click reliably, so we don't carry
 *    that workaround. The dismissal still runs through
 *    {@link dismissAfterGesture} (a setTimeout 0) as cheap insurance that
 *    the unmount always lands after the touch fully settles. SheetButton,
 *    by contrast, runs its *action* inline (synchronously): clipboard
 *    writes/reads and window.open must stay in the gesture task for WebKit
 *    to honour them (see {@link dismissAfterGesture}). The long-press
 *    recognizer is touch-gated, so
 *    the sheets only exist on touch devices; no keyboard-activation path to
 *    lose.
 *
 * Opening a sheet also closes the on-screen keyboard (see effect below).
 *
 * The panel (with its role/aria-label) is passed as children.
 */
import type { ComponentChildren } from 'preact'
import { createPortal } from 'preact/compat'
import { useEffect } from 'preact/hooks'
import { pushError, pushToast } from './toasts'

/**
 * Defer a sheet's *dismissal* (the DOM unmount) to the next macrotask
 * rather than running it inline in the click handler. click already fires
 * after the touch ends, so this is belt-and-suspenders: it guarantees the
 * sheet's DOM is removed only once the gesture has fully settled, so
 * removing it can never tear down the terminal's iOS momentum-scroll layer
 * mid-gesture — the regression we hit when these dismissed on pointerup.
 * Cheap to keep; see point 3 in the file header.
 *
 * Only the *dismissal* is deferred, never the action. Clipboard writes/reads
 * and window.open must run synchronously inside the click task: WebKit/Safari
 * ties them to the live user gesture and does not honour the spec's
 * "transient activation survives a task" model for them, so a setTimeout(0)
 * gets the write/open rejected or the popup blocked. SheetButton runs its
 * onActivate inline for that reason.
 */
function dismissAfterGesture(fn: () => void): void {
  setTimeout(fn, 0)
}

export function SheetBackdrop({ onClose, children }: {
  onClose: () => void
  children: ComponentChildren
}) {
  // Close the keyboard when a sheet opens by blurring the terminal's
  // focused hidden textarea. While it holds focus iOS locks selection to
  // it — you can't select the sheet's text — and draws its insertion caret
  // over everything; blurring releases both. You don't need the keyboard
  // to copy/paste, and dropping it gives the sheet more room.
  useEffect(() => {
    (document.activeElement as HTMLElement | null)?.blur()
  }, [])

  return createPortal(
    <div
      class="modal-backdrop action-sheet-backdrop"
      onClick={ev => {
        ev.stopPropagation()
        if (ev.target === ev.currentTarget) dismissAfterGesture(onClose)
      }}
    >
      {children}
    </div>,
    document.body,
  )
}

/** A button inside a sheet. `primary` gives it the accent fill; `quiet`
 * the de-emphasised treatment for a dismiss (Close) that sits apart from
 * the real actions. Runs onActivate synchronously on click so gesture-gated
 * actions (clipboard, window.open) keep their user activation on WebKit —
 * see {@link dismissAfterGesture}. */
export function SheetButton({ primary = false, quiet = false, onActivate, children }: {
  primary?: boolean
  quiet?: boolean
  onActivate: () => void
  children: ComponentChildren
}) {
  return (
    <button
      type="button"
      class={`sheet-btn${primary ? ' sheet-btn-primary' : ''}${quiet ? ' sheet-btn-quiet' : ''}`}
      onClick={ev => { ev.stopPropagation(); onActivate() }}
    >
      {children}
    </button>
  )
}

/** A {@link SheetButton} that copies `text` to the clipboard, closes the
 * sheet immediately, and confirms via a toast (`Copied to clipboard` /
 * `Copy failed`). Confirmation is decoupled from the surface: the sheet
 * doesn't need to linger (or guess an auto-close delay) just to show
 * feedback. The clipboard write is initiated synchronously inside the
 * click task — WebKit gesture-gates it (see {@link dismissAfterGesture}) —
 * and the toast fires when the promise settles. Centralises the copy
 * behaviour both the link and text sheets need. */
export function CopyButton({ label, text, onClose }: {
  label: string
  text: string
  onClose: () => void
}) {
  const copy = () => {
    navigator.clipboard.writeText(text).then(
      () => pushToast('info', 'Copied to clipboard'),
      () => pushError('Copy failed'),
    )
    onClose()
  }
  return <SheetButton onActivate={copy}>{label}</SheetButton>
}
