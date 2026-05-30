/**
 * Touch helpers for the terminal: focus management and scroll-pan.
 * Extracted for testability; used by terminal.tsx.
 */
import { useEffect } from 'preact/hooks'
import type { RefObject } from 'preact'
import type { WTerm } from '@wterm/dom'
import type { TerminalSize } from './terminal-io'
import { selectAllAndCopy } from './selection'

/**
 * Returns true if the terminal should be focused on touchend.
 * Skip focus when the touch was a drag or a long-press.
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
 * Returns true when the position dance should be skipped (textarea already focused).
 * Extracted for unit-testability.
 */
export function shouldSkipPositionDance(alreadyFocused: boolean): boolean {
  return alreadyFocused
}

/**
 * Focus the wterm element. On mobile, wterm uses a textarea internally;
 * term.focus() delegates to it. No position dance needed — DOM rendering
 * has no canvas click-hit issues.
 */
export function focusTerminalInput(term: WTerm | null): void {
  if (!term) return
  term.focus()
}

/**
 * Attaches touch-pan scroll listeners to the shell container, enabling
 * natural scroll when the terminal viewport is smaller than the PTY size.
 * All state is accessed via refs so the effect is stable (empty dep array).
 */
export function useTouchPan(
  shellRef: RefObject<HTMLDivElement | null>,
  termRef: RefObject<WTerm | null>,
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
        // Long-press: select all and copy using DOM selection.
        const term = termRef.current
        if (term) selectAllAndCopy(term.element)
        state.active = false
        state.moved  = false
        return
      }

      if (state.active && shouldFocusOnTouchEnd({ moved: state.moved, wasLongPress: state.wasLongPress })) {
        // Tap: focus and scroll to bottom. DOM rendering handles click events
        // natively — no canvas click synthesis needed.
        const term = termRef.current
        if (term) {
          focusTerminalInput(term)
          setTimeout(() => {
            // Scroll terminal element to bottom
            const el = term.element
            el.scrollTop = el.scrollHeight
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
