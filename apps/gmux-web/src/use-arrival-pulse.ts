import { useEffect, useRef, useState } from 'preact/hooks'

/**
 * Fires the "arriving" CSS animation whenever the dot transitions to 'unread'.
 *
 * Triggers on any state → unread transition (none, active, working, error).
 *
 * Optional `generation` parameter: when provided, a change in generation
 * while the state is already 'unread' re-fires the animation. Used by the
 * mobile hamburger badge so that a second session becoming unread still
 * draws attention even though the aggregate state stays 'unread'.
 */
export function useArrivalPulse(
  currentState: 'working' | 'error' | 'unread' | 'active' | 'fading' | 'none',
  generation?: number,
): 'arriving' | null {
  const prevStateRef = useRef(currentState)
  const prevGenRef = useRef(generation)
  const [arriving, setArriving] = useState(false)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const fire = () => {
    if (timerRef.current) clearTimeout(timerRef.current)
    setArriving(true)
    // Match the CSS animation duration (520ms) + small buffer
    timerRef.current = setTimeout(() => {
      setArriving(false)
      timerRef.current = null
    }, 560)
  }

  const cancel = () => {
    if (timerRef.current) {
      clearTimeout(timerRef.current)
      timerRef.current = null
    }
    setArriving(false)
  }

  useEffect(() => {
    const prevState = prevStateRef.current
    const prevGen = prevGenRef.current
    prevStateRef.current = currentState
    prevGenRef.current = generation

    if (currentState === 'unread') {
      // State just transitioned to unread
      if (prevState !== 'unread') {
        fire()
        return
      }
      // State was already unread but generation increased (new unread session)
      if (generation !== undefined && prevGen !== undefined && generation > prevGen) {
        fire()
        return
      }
    } else {
      // Moved away from unread, cancel any in-flight animation
      cancel()
    }
  }, [currentState, generation])

  // Cleanup on unmount
  useEffect(() => () => {
    if (timerRef.current) clearTimeout(timerRef.current)
  }, [])

  return arriving ? 'arriving' : null
}
