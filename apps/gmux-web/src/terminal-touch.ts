/**
 * Touch helpers for the terminal: focus management, scroll-pan, and the
 * useTouchPan hook. Extracted for testability; used by terminal.tsx.
 */
import { useEffect } from 'preact/hooks'
import type { RefObject } from 'preact'
import type { Terminal } from 'ghostty-web'
import type { TerminalSize } from './terminal-io'
import { selectionToText } from './selection'

/**
 * Returns true if the terminal textarea should be focused on touchend.
 * We skip focus when:
 *  - the touch was a drag (`moved`), or
 *  - the OS long-press text-selection gesture fired (`wasLongPress`),
 *    because focusing the textarea would collapse the selection.
 */
export function shouldFocusOnTouchEnd({
  moved,
  wasLongPress,
}: {
  moved: boolean
  wasLongPress: boolean
}): boolean {
  return !moved && !wasLongPress
}

/**
 * Returns true when the already-focused guard in focusTerminalInput should
 * skip the position dance. Extracted for unit-testability.
 */
export function shouldSkipPositionDance(alreadyFocused: boolean): boolean {
  return alreadyFocused
}

/**
 * Focus the terminal's hidden textarea input, with a mobile workaround:
 * briefly repositions the textarea to the bottom of the screen so the
 * on-screen keyboard appears at the correct anchor point.
 *
 * Fix — keyboard flicker: record whether the textarea was already focused
 * BEFORE calling term.focus(). If it was, skip the position-dance entirely.
 * Re-anchoring a focused textarea (move + focus again) is what caused iOS
 * to dismiss and re-show the keyboard on every resize event.
 */
export function focusTerminalInput(term: Terminal | null): void {
  if (!term) return

  const textarea = term.textarea
  if (!textarea) return

  const isTouchDevice = window.matchMedia('(pointer: coarse)').matches
    || navigator.maxTouchPoints > 0

  if (!isTouchDevice) {
    term.focus()
    return
  }

  // Capture focused state BEFORE term.focus() changes it.
  const alreadyFocused = document.activeElement === textarea

  term.focus()

  // Skip the position dance if the textarea was already the active element.
  // Doing the dance re-anchors the keyboard on iOS, causing a visible flash.
  if (shouldSkipPositionDance(alreadyFocused)) return

  const prev = {
    position: textarea.style.position,
    left:     textarea.style.left,
    bottom:   textarea.style.bottom,
    top:      textarea.style.top,
    width:    textarea.style.width,
    height:   textarea.style.height,
    opacity:  textarea.style.opacity,
    zIndex:   textarea.style.zIndex,
  }

  textarea.style.position = 'fixed'
  textarea.style.left     = '0'
  textarea.style.bottom   = '0'
  textarea.style.top      = 'auto'
  textarea.style.width    = '1px'
  textarea.style.height   = '1px'
  textarea.style.opacity  = '0.01'
  textarea.style.zIndex   = '-1'
  textarea.focus({ preventScroll: true })

  requestAnimationFrame(() => {
    textarea.style.position = prev.position
    textarea.style.left     = prev.left
    textarea.style.bottom   = prev.bottom
    textarea.style.top      = prev.top
    textarea.style.width    = prev.width
    textarea.style.height   = prev.height
    textarea.style.opacity  = prev.opacity
    textarea.style.zIndex   = prev.zIndex
  })
}

/**
 * Attaches touch-pan scroll listeners to the shell container, enabling
 * natural scroll when the terminal viewport is smaller than the PTY size.
 * All state is accessed via refs so the effect is stable (empty dep array).
 *
 * @param containerRef  Ref to the `.terminal-container` div (child of shell).
 *   When provided, a tap synthesizes a `click` MouseEvent on the canvas so
 *   ghostty-web's link-click handler fires. Without this, the canvas's own
 *   `touchend → e.preventDefault()` kills the browser-synthesized click and
 *   links never activate on mobile.
 *
 *   Long-press selects all terminal content and copies it to the clipboard,
 *   giving mobile users a reliable copy path without needing drag selection.
 */
export function useTouchPan(
  shellRef: RefObject<HTMLDivElement | null>,
  termRef: RefObject<Terminal | null>,
  viewportSizeRef: RefObject<TerminalSize | null>,
  ptySizeRef: RefObject<TerminalSize | null>,
  containerRef?: RefObject<HTMLDivElement | null>,
): void {
  useEffect(() => {
    const shell = shellRef.current
    if (!shell) return

    const isInteractiveTarget = (target: EventTarget | null) =>
      target instanceof HTMLElement &&
      !!target.closest('button, input, textarea, select, a, label, [role="button"]')

    const state = {
      active: false,
      moved: false,
      startX: 0,
      startY: 0,
      startScrollLeft: 0,
      startScrollTop: 0,
      longPressTimer: null as ReturnType<typeof setTimeout> | null,
      wasLongPress: false,
    }

    const handleTouchStart = (ev: TouchEvent) => {
      if (ev.touches.length !== 1 || isInteractiveTarget(ev.target)) {
        state.active = false; state.moved = false; return
      }
      const host = shellRef.current
      if (!host) { state.active = false; state.moved = false; return }

      state.active       = true
      state.moved        = false
      state.wasLongPress = false
      state.startX           = ev.touches[0].clientX
      state.startY           = ev.touches[0].clientY
      state.startScrollLeft  = host.scrollLeft
      state.startScrollTop   = host.scrollTop

      if (state.longPressTimer !== null) clearTimeout(state.longPressTimer)
      state.longPressTimer = setTimeout(() => {
        state.longPressTimer = null
        state.wasLongPress   = true
      }, 400)
    }

    const handleTouchMove = (ev: TouchEvent) => {
      if (!state.active || ev.touches.length !== 1) return
      const host = shellRef.current
      if (!host) return

      const touch  = ev.touches[0]
      const deltaX = touch.clientX - state.startX
      const deltaY = touch.clientY - state.startY
      if (Math.abs(deltaX) > 6 || Math.abs(deltaY) > 6) {
        state.moved = true
        if (state.longPressTimer !== null) {
          clearTimeout(state.longPressTimer)
          state.longPressTimer = null
        }
      }

      const vp  = viewportSizeRef.current
      const pty = ptySizeRef.current
      if (vp && pty && vp.cols === pty.cols && vp.rows === pty.rows) return

      const canScrollX = host.scrollWidth  > host.clientWidth
      const canScrollY = host.scrollHeight > host.clientHeight
      if (!canScrollX && !canScrollY) return

      if (canScrollX) host.scrollLeft = state.startScrollLeft - deltaX
      if (canScrollY) host.scrollTop  = state.startScrollTop  - deltaY
      ev.preventDefault()
      ev.stopPropagation()
    }

    const handleTouchEnd = (ev: TouchEvent) => {
      if (state.longPressTimer !== null) {
        clearTimeout(state.longPressTimer)
        state.longPressTimer = null
      }

      if (state.active && state.wasLongPress) {
        // Long-press: select all visible content and copy to clipboard.
        // Skip textarea focus so the keyboard stays hidden and the selection
        // highlight remains visible.
        const term = termRef.current
        if (term) {
          term.selectAll()
          const text = selectionToText(term)
          if (text) navigator.clipboard.writeText(text).catch(() => {})
        }
        state.active = false
        state.moved  = false
        return
      }

      if (state.active && shouldFocusOnTouchEnd({ moved: state.moved, wasLongPress: state.wasLongPress })) {
        // Tap: synthesize a click on the canvas so ghostty-web's link-click
        // handler fires. The canvas's own touchend calls e.preventDefault()
        // which kills the browser-generated synthetic click, so we dispatch
        // our own MouseEvent directly on the canvas element.
        if (containerRef?.current && ev.changedTouches.length > 0) {
          const canvas = containerRef.current.querySelector('canvas')
          if (canvas) {
            const touch = ev.changedTouches[0]
            canvas.dispatchEvent(new MouseEvent('click', {
              clientX: touch.clientX,
              clientY: touch.clientY,
              bubbles: true,
              cancelable: true,
              button: 0,
              view: window,
            }))
          }
        }

        const term = termRef.current
        if (term) {
          focusTerminalInput(term)
          setTimeout(() => {
            term.scrollToBottom()
            const host = shellRef.current
            if (host) { host.scrollTop = host.scrollHeight; host.scrollLeft = 0 }
          }, 0)
        }
      }
      state.active = false
      state.moved  = false
    }

    const clearState = () => {
      if (state.longPressTimer !== null) {
        clearTimeout(state.longPressTimer)
        state.longPressTimer = null
      }
      state.active       = false
      state.moved        = false
      state.wasLongPress = false
    }

    shell.addEventListener('touchstart', handleTouchStart, { capture: true, passive: false })
    shell.addEventListener('touchmove',  handleTouchMove,  { capture: true, passive: false })
    shell.addEventListener('touchend',   handleTouchEnd,   true)
    shell.addEventListener('touchcancel', clearState,      true)

    return () => {
      shell.removeEventListener('touchstart', handleTouchStart, true)
      shell.removeEventListener('touchmove',  handleTouchMove,  true)
      shell.removeEventListener('touchend',   handleTouchEnd,   true)
      shell.removeEventListener('touchcancel', clearState,      true)
    }
  }, []) // stable — all state accessed via refs at handler-call time
}
