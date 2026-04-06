/**
 * Presence hook: WebSocket connection to the daemon for notifications,
 * tab title badge, idle detection, and visibility/focus reporting.
 */

import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { connectPresence } from './presence'
import type { NotifyMessage, CancelMessage } from './presence'
import type { Session } from './types'
import type { NotifPermission } from './sidebar'

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

interface UsePresenceOptions {
  selectedId: string | null
  sessions: Session[]
  /** Called when the user clicks a notification that has a session_id. */
  onNotificationClick: (sessionId: string) => void
}

interface UsePresenceResult {
  notifPermission: NotifPermission
  requestNotifPermission: () => void
}

export function usePresence({
  selectedId,
  sessions,
  onNotificationClick,
}: UsePresenceOptions): UsePresenceResult {
  const activeNotifsRef = useRef<Map<string, Notification>>(new Map())
  const presenceRef = useRef<ReturnType<typeof connectPresence> | null>(null)
  const lastInteractionRef = useRef(Date.now() / 1000)

  // Notification permission. Not reactive, so we keep a tick to force
  // a re-read after requestPermission() resolves.
  const [, forceNotifPermUpdate] = useState(0)
  const notifPermission: NotifPermission = USE_MOCK
    ? 'granted'
    : ('Notification' in window ? Notification.permission : 'unavailable')

  // Stable refs for callbacks so the presence effect doesn't re-subscribe.
  const onNotifClickRef = useRef(onNotificationClick)
  onNotifClickRef.current = onNotificationClick
  const sessionsRef = useRef(sessions)
  sessionsRef.current = sessions

  // Show a notification when the daemon tells us to.
  const handleNotify = useCallback((msg: NotifyMessage) => {
    if (!('Notification' in window) || Notification.permission !== 'granted') return
    const n = new Notification(msg.title, {
      body: msg.body,
      tag: msg.tag,
      icon: '/favicon.svg',
    })
    activeNotifsRef.current.set(msg.id, n)
    n.onclose = () => activeNotifsRef.current.delete(msg.id)
    n.onclick = () => {
      window.focus()
      if (msg.session_id) onNotifClickRef.current(msg.session_id)
      n.close()
    }
  }, [])

  // Dismiss a notification when the daemon tells us to.
  const handleCancel = useCallback((msg: CancelMessage) => {
    const n = activeNotifsRef.current.get(msg.id)
    if (n) { n.close(); activeNotifsRef.current.delete(msg.id) }
  }, [])

  // Connect presence WebSocket on mount.
  useEffect(() => {
    const p = connectPresence({ onNotify: handleNotify, onCancel: handleCancel })
    presenceRef.current = p
    return () => { p.close(); presenceRef.current = null }
  }, [handleNotify, handleCancel])

  // Track last user interaction for idle detection.
  useEffect(() => {
    const update = () => { lastInteractionRef.current = Date.now() / 1000 }
    const events = ['mousemove', 'keydown', 'touchstart', 'scroll'] as const
    events.forEach(e => document.addEventListener(e, update, { passive: true }))
    return () => events.forEach(e => document.removeEventListener(e, update))
  }, [])

  // Report state changes to the daemon.
  const reportPresence = useCallback(() => {
    presenceRef.current?.sendState({
      visibility: document.visibilityState,
      focused: document.hasFocus(),
      selected_session_id: selectedId,
      last_interaction: lastInteractionRef.current,
    })
  }, [selectedId])

  // Report whenever visibility, focus, or selected session changes.
  useEffect(() => { reportPresence() }, [reportPresence])
  useEffect(() => {
    const report = () => reportPresence()
    document.addEventListener('visibilitychange', report)
    window.addEventListener('focus', report)
    window.addEventListener('blur', report)
    const heartbeat = setInterval(report, 30_000)
    return () => {
      document.removeEventListener('visibilitychange', report)
      window.removeEventListener('focus', report)
      window.removeEventListener('blur', report)
      clearInterval(heartbeat)
    }
  }, [reportPresence])

  // Tab title badge.
  useEffect(() => {
    const count = sessions.filter(s =>
      s.id !== selectedId && s.alive && s.unread
    ).length
    document.title = count > 0 ? `(${count}) gmux` : 'gmux'
  }, [sessions, selectedId])

  const requestNotifPermission = useCallback(async () => {
    await Notification.requestPermission()
    forceNotifPermUpdate(n => n + 1)
    presenceRef.current?.sendPermission(Notification.permission)
  }, [])

  return { notifPermission, requestNotifPermission }
}
