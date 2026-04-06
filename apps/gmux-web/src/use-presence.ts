/**
 * Presence hook: WebSocket connection to the daemon for notifications,
 * tab title badge, idle detection, and visibility/focus reporting.
 *
 * Reads selectedId and sessions from the store (signals). The only
 * prop-driven input is the notification click callback (needs routing).
 */

import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { connectPresence } from './presence'
import type { NotifyMessage, CancelMessage } from './presence'
import { selectedId, sessions, navigateToSession } from './store'
import type { NotifPermission } from './sidebar'

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

interface UsePresenceResult {
  notifPermission: NotifPermission
  requestNotifPermission: () => void
}

export function usePresence(): UsePresenceResult {
  const activeNotifsRef = useRef<Map<string, Notification>>(new Map())
  const presenceRef = useRef<ReturnType<typeof connectPresence> | null>(null)
  const lastInteractionRef = useRef(Date.now() / 1000)

  const [, forceNotifPermUpdate] = useState(0)
  const notifPermission: NotifPermission = USE_MOCK
    ? 'granted'
    : ('Notification' in window ? Notification.permission : 'unavailable')

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
      if (msg.session_id) navigateToSession(msg.session_id)
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
  // Read signals inside the callback; useCallback has no deps since
  // signal reads are always current.
  const reportPresence = useCallback(() => {
    presenceRef.current?.sendState({
      visibility: document.visibilityState,
      focused: document.hasFocus(),
      selected_session_id: selectedId.value,
      last_interaction: lastInteractionRef.current,
    })
  }, [])

  // Report on visibility/focus changes + heartbeat.
  // Also re-report whenever selectedId changes.
  useEffect(() => {
    reportPresence()
  }, [selectedId.value, reportPresence])

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
    const sel = selectedId.value
    const count = sessions.value.filter(s =>
      s.id !== sel && s.alive && s.unread
    ).length
    document.title = count > 0 ? `(${count}) gmux` : 'gmux'
  }, [sessions.value, selectedId.value])

  const requestNotifPermission = useCallback(async () => {
    await Notification.requestPermission()
    forceNotifPermUpdate(n => n + 1)
    presenceRef.current?.sendPermission(Notification.permission)
  }, [])

  return { notifPermission, requestNotifPermission }
}
