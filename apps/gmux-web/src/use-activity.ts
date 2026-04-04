import { useCallback, useEffect, useRef, useState } from 'preact/hooks'

/**
 * Tracks which sessions are "recently active" (produced terminal output
 * within the last few seconds). Driven by session-activity SSE events
 * from the backend.
 *
 * Returns:
 * - isActive(id): whether a session is currently showing the active indicator
 * - isFading(id): whether the active indicator is in its fade-out phase
 * - handleActivity(id): call when a session-activity event arrives
 * - activityVersion: counter that increments on every activity state change;
 *   use as a useMemo dependency to recompute derived state
 */
export function useActivityTracker(fadeMs = 3000, fadeOutMs = 800) {
  // Map of sessionId → timeout handle. Presence in activeRef = active.
  // Presence in fadingRef = in fade-out phase (visible but dimming).
  const activeRef = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map())
  const fadingRef = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map())
  const [activityVersion, tick] = useState(0)

  const handleActivity = useCallback((sessionId: string) => {
    const active = activeRef.current
    const fading = fadingRef.current

    // Cancel any existing timers for this session.
    const existingActive = active.get(sessionId)
    if (existingActive) clearTimeout(existingActive)
    const existingFading = fading.get(sessionId)
    if (existingFading) { clearTimeout(existingFading); fading.delete(sessionId) }

    active.set(sessionId, setTimeout(() => {
      active.delete(sessionId)
      // Enter fade-out phase.
      fading.set(sessionId, setTimeout(() => {
        fading.delete(sessionId)
        tick(n => n + 1)
      }, fadeOutMs))
      tick(n => n + 1)
    }, fadeMs))

    tick(n => n + 1) // show immediately
  }, [fadeMs, fadeOutMs])

  const isActive = useCallback((sessionId: string): boolean => {
    return activeRef.current.has(sessionId)
  }, [])

  const isFading = useCallback((sessionId: string): boolean => {
    return fadingRef.current.has(sessionId)
  }, [])

  // Cleanup on unmount
  useEffect(() => () => {
    for (const t of activeRef.current.values()) clearTimeout(t)
    for (const t of fadingRef.current.values()) clearTimeout(t)
  }, [])

  return { isActive, isFading, handleActivity, activityVersion }
}
