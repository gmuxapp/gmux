/**
 * Touch helpers for the terminal: focus management, scroll-pan, and the
 * useTouchPan hook. Extracted for testability; used by terminal.tsx.
 */
import { useEffect } from 'preact/hooks'
import type { RefObject } from 'preact'
import type { Terminal } from 'ghostty-web'
import type { TerminalSize } from './terminal-io'

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
 * Focus the terminal's hidden textarea input, with a mobile workaround:
 * briefly repositions the textarea to the bottom of the screen so the
 * on-screen keyboard appears at the correct anchor point.
 */
export function focusTerminalInput(term: Terminal | null): void {
  if (!term) return

  term.focus()

  const textarea = term.textarea
  if (!textarea) return

  const isTouchDevice = window.matchMedia('(pointer: coarse)').matches
    || navigator.maxTouchPoints > 0
  if (!isTouchDevice) return

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
 */
export function useTouchPan(
  shellRef: RefObject<HTMLDivElement | null>,
  termRef: RefObject<Terminal | null>,
  viewportSizeRef: RefObject<TerminalSize | null>,
  ptySizeRef: RefObject<TerminalSize | null>,
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

    const handleTouchEnd = () => {
      if (state.longPressTimer !== null) {
        clearTimeout(state.longPressTimer)
        state.longPressTimer = null
      }
      if (state.active && shouldFocusOnTouchEnd({ moved: state.moved, wasLongPress: state.wasLongPress })) {
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
