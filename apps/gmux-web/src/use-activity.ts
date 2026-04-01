import { useCallback, useEffect, useRef, useState } from 'preact/hooks'

/**
 * Tracks which sessions are "recently active" (produced terminal output
 * within the last few seconds). Driven by session-activity SSE events
 * from the backend.
 *
 * Returns:
 * - isActive(id): whether a session is currently showing the active indicator
 * - handleActivity(id): call when a session-activity event arrives
 * - activityVersion: counter that increments on every activity state change;
 *   use as a useMemo dependency to recompute derived state
 */
export function useActivityTracker(fadeMs = 3000) {
  // Map of sessionId → timeout handle. Presence = active.
  const timersRef = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map())
  const [activityVersion, tick] = useState(0)

  const handleActivity = useCallback((sessionId: string) => {
    const timers = timersRef.current
    const existing = timers.get(sessionId)
    if (existing) clearTimeout(existing)

    timers.set(sessionId, setTimeout(() => {
      timers.delete(sessionId)
      tick(n => n + 1) // trigger re-render when activity expires
    }, fadeMs))

    tick(n => n + 1) // trigger re-render to show immediately
  }, [fadeMs])

  const isActive = useCallback((sessionId: string): boolean => {
    return timersRef.current.has(sessionId)
  }, [])

  // Cleanup on unmount
  useEffect(() => () => {
    for (const t of timersRef.current.values()) clearTimeout(t)
  }, [])

  return { isActive, handleActivity, activityVersion }
}
