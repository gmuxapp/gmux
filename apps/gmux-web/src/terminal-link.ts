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

/**
 * Internal Linkifier surface we rely on (stable in our pinned fork).
 * The range is 1-based; y is an absolute buffer row (scrollback
 * included). `text` is the link's URI for both built-in providers:
 * WebLinksAddon stores the regex match, OscLinkProvider stores the
 * OSC 8 URI (not the visible label).
 */
interface InternalLink {
  text: string
  range: {
    start: { x: number, y: number }
    end: { x: number, y: number }
  }
}

interface CoreWithLinkifier {
  _core?: { linkifier?: { currentLink?: { link: InternalLink } } }
}

/** A link resolved under a touch point. */
export interface LinkInfo {
  /** The target URI (what activation would open). */
  uri: string
  /** The text painted in the terminal for this link. Equals `uri` for
   * plain-text URLs; differs for OSC 8 hyperlinks, where the buffer
   * shows a label and the URI is hidden — surfacing both lets the user
   * inspect the real target before opening. */
  label: string
}

interface ResolvedLink {
  screen: Element
  init: MouseEventInit
  link: InternalLink
}

function resolveLinkAtPoint(term: Terminal, clientX: number, clientY: number): ResolvedLink | null {
  const screen = term.element?.querySelector('.xterm-screen')
  if (!screen) return null

  const linkifier = (term as unknown as CoreWithLinkifier)._core?.linkifier
  if (!linkifier) return null

  // Non-bubbling on purpose: the Linkifier listens on the screen
  // element itself, so it receives these regardless (events always
  // fire on their target), while listeners on ancestors never see
  // them. That keeps the handshake invisible to xterm's MouseService
  // (whose element-level mousedown handler calls focus(), which would
  // open the on-screen keyboard), selection handling, and PTY mouse
  // reporting.
  const init: MouseEventInit = { bubbles: false, clientX, clientY }

  // Hover step of the handshake. Link providers reply synchronously,
  // so currentLink is settled by the time dispatchEvent returns.
  screen.dispatchEvent(new MouseEvent('mousemove', init))
  const link = linkifier.currentLink?.link
  if (!link) return null

  return { screen, init, link }
}

/** Read the text the link's range covers in the buffer (its visible
 * label). Falls back to the URI if any row is unavailable. */
function labelForLink(term: Terminal, link: InternalLink): string {
  const { start, end } = link.range
  let label = ''
  for (let y = start.y; y <= end.y; y++) {
    const line = term.buffer.active.getLine(y - 1)
    if (!line) return link.text
    const fromCol = y === start.y ? start.x - 1 : 0
    const toCol = y === end.y ? end.x : term.cols
    label += line.translateToString(false, fromCol, toCol)
  }
  return label
}

/**
 * Query-only: resolve the link under (clientX, clientY) without
 * activating it. Side effects are limited to a synthetic hover.
 * Returns null when there is no link, the renderer hasn't measured
 * yet, or the internal linkifier is unavailable.
 */
export function linkAtPoint(term: Terminal, clientX: number, clientY: number): LinkInfo | null {
  const resolved = resolveLinkAtPoint(term, clientX, clientY)
  if (!resolved) return null
  return { uri: resolved.link.text, label: labelForLink(term, resolved.link) }
}

/**
 * If a link is under (clientX, clientY), activate it via the Linkifier
 * and return true. The mousedown/mouseup pair is only dispatched when
 * a link was resolved, so plain taps inject nothing.
 */
export function openLinkAtPoint(term: Terminal, clientX: number, clientY: number): boolean {
  const resolved = resolveLinkAtPoint(term, clientX, clientY)
  if (!resolved) return false

  // Press and release at the same point → Linkifier calls
  // link.activate() (WebLinksAddon's handler or the OSC 8 handler).
  const { screen, init } = resolved
  screen.dispatchEvent(new MouseEvent('mousedown', { ...init, button: 0, buttons: 1 }))
  screen.dispatchEvent(new MouseEvent('mouseup', { ...init, button: 0 }))
  return true
}
