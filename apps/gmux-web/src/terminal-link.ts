/**
 * Open the link under a touch point by driving xterm's Linkifier with a
 * synthetic mouse-event sequence.
 *
 * Why this exists: xterm activates links via a 3-step handshake on the
 * screen element — `mousemove` resolves the link under the pointer into
 * `Linkifier._currentLink` (link providers reply synchronously),
 * `mousedown` snapshots it, and `mouseup` activates it if both match.
 * On mobile, the browser-synthesized cascade after touchend is supposed
 * to provide these events, but it's unreliable:
 *
 * - iOS cancels the rest of the cascade if the DOM changes during
 *   mouseover/mousemove (the "content change rule"). Our tap handler
 *   focuses the hidden textarea, which mutates styles and opens the
 *   keyboard — so on iOS the cascade always died and links never worked.
 * - The keyboard-open viewport resize triggers a terminal resize, and
 *   the Linkifier clears its current link on resize, racing whatever
 *   events do arrive (the Android flakiness).
 *
 * Instead, the tap handler calls preventDefault() on touchend (which
 * per spec suppresses the browser's synthesized cascade entirely) and
 * we dispatch the handshake ourselves, synchronously, before any
 * focus/keyboard/resize can interfere. WebLinksAddon stays the single
 * source of truth for link detection (same regex as desktop), and
 * OSC 8 hyperlinks work too since OscLinkProvider is also registered.
 *
 * Considered and rejected: expressing the keyboard toggle as a
 * lowest-priority catch-all link provider, so the Linkifier's priority
 * resolution would pick "real link" vs "toggle keyboard" itself. Link
 * providers answer "what's at this cell", not "what should a tap do":
 * they aren't modality-aware (desktop clicks would activate the
 * catch-all too), it would require dispatching mousedown/mouseup on
 * every tap (feeding selection and PTY mouse reporting on plain taps),
 * and tap-vs-pan-vs-long-press discrimination lives in the touch
 * handler regardless. The tap decision stays a flat branch there.
 */
import type { Terminal } from '@xterm/xterm'

/** Internal Linkifier surface we rely on (stable in our pinned fork). */
interface CoreWithLinkifier {
  _core?: { linkifier?: { currentLink?: unknown } }
}

/**
 * If a link is under (clientX, clientY), activate it via the Linkifier
 * and return true. Returns false (with no side effects beyond a hover)
 * when there is no link, the renderer hasn't measured yet, or the
 * internal linkifier is unavailable.
 *
 * The mousedown/mouseup pair is only dispatched when a link was
 * resolved, and all events are non-bubbling, so the handshake never
 * reaches selection handling, PTY mouse reporting, or the focus-
 * grabbing MouseService handler on the outer element.
 */
export function openLinkAtPoint(term: Terminal, clientX: number, clientY: number): boolean {
  const screen = term.element?.querySelector('.xterm-screen')
  if (!screen) return false

  const linkifier = (term as unknown as CoreWithLinkifier)._core?.linkifier
  if (!linkifier) return false

  // Non-bubbling on purpose: the Linkifier listens on the screen
  // element itself, so it receives these regardless (events always
  // fire on their target), while listeners on ancestors never see
  // them. That keeps the handshake invisible to xterm's MouseService
  // (whose element-level mousedown handler calls focus(), which would
  // open the on-screen keyboard), selection handling, and PTY mouse
  // reporting.
  const init: MouseEventInit = { bubbles: false, clientX, clientY }

  // Step 1: hover. Link providers reply synchronously, so currentLink
  // is settled by the time dispatchEvent returns.
  screen.dispatchEvent(new MouseEvent('mousemove', init))
  if (!linkifier.currentLink) return false

  // Steps 2+3: press and release at the same point → Linkifier calls
  // link.activate() (WebLinksAddon's handler or the OSC 8 handler).
  screen.dispatchEvent(new MouseEvent('mousedown', { ...init, button: 0, buttons: 1 }))
  screen.dispatchEvent(new MouseEvent('mouseup', { ...init, button: 0 }))
  return true
}
