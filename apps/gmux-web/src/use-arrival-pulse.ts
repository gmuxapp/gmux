import { useEffect, useRef, useState } from 'preact/hooks'

/**
 * Detects a working→unread or error→unread transition and returns
 * 'arriving' for the duration of the CSS animation, then clears itself.
 *
 * Shared between the sidebar session dots and the mobile hamburger badge.
 */
export function useArrivalPulse(
  currentState: 'working' | 'error' | 'unread' | 'none',
): 'arriving' | null {
  const prevRef = useRef(currentState)
  const [arriving, setArriving] = useState(false)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    const prev = prevRef.current
    prevRef.current = currentState

    if ((prev === 'working' || prev === 'error') && currentState === 'unread') {
      // Cancel any in-flight timer from a previous arrival
      if (timerRef.current) clearTimeout(timerRef.current)
      setArriving(true)
      // Match the CSS animation duration (520ms) + small buffer
      timerRef.current = setTimeout(() => {
        setArriving(false)
        timerRef.current = null
      }, 560)
    } else if (currentState !== 'unread') {
      // If state changed away from unread, cancel any pending arrival
      if (timerRef.current) {
        clearTimeout(timerRef.current)
        timerRef.current = null
      }
      setArriving(false)
    }
  }, [currentState])

  // Cleanup on unmount
  useEffect(() => () => {
    if (timerRef.current) clearTimeout(timerRef.current)
  }, [])

  return arriving ? 'arriving' : null
}
