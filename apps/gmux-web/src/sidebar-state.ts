/**
 * Sidebar visibility state — stored in localStorage.
 *
 * Positive set: we store what IS visible. New items auto-show.
 * Frontend owns this state; gmuxd just provides the session catalog.
 */

import type { Session } from './types'

const STORAGE_KEY = 'gmux-sidebar-state'

export interface SidebarState {
  /** cwds of folders visible in sidebar */
  visibleFolders: string[]
  /** per-cwd resume_keys that are promoted out of "show more" */
  visibleSessions: Record<string, string[]>
}

function load(): SidebarState {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw) return JSON.parse(raw)
  } catch { /* corrupt or missing — start fresh */ }
  return { visibleFolders: [], visibleSessions: {} }
}

function save(state: SidebarState) {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(state))
}

export function createSidebarState() {
  let state = load()
  const listeners = new Set<() => void>()

  function notify() {
    save(state)
    for (const fn of listeners) fn()
  }

  return {
    /** Subscribe to state changes */
    subscribe(fn: () => void) {
      listeners.add(fn)
      return () => { listeners.delete(fn) }
    },

    getState(): Readonly<SidebarState> {
      return state
    },

    /**
     * Sync with current session list from gmuxd.
     * Auto-adds folders with live sessions, auto-adds live sessions to visible.
     */
    syncSessions(sessions: Session[]) {
      let changed = false
      for (const s of sessions) {
        const cwd = s.cwd ?? '~'
        if (s.alive) {
          // Auto-show folder for live sessions
          if (!state.visibleFolders.includes(cwd)) {
            state.visibleFolders.push(cwd)
            changed = true
          }
          // Auto-promote live sessions (by resume_key if available)
          const key = s.resume_key ?? s.id
          const arr = state.visibleSessions[cwd] ??= []
          if (!arr.includes(key)) {
            arr.push(key)
            changed = true
          }
        }
      }
      if (changed) notify()
    },

    /** Show a folder in the sidebar */
    showFolder(cwd: string) {
      if (!state.visibleFolders.includes(cwd)) {
        state.visibleFolders.push(cwd)
        notify()
      }
    },

    /** Hide a folder from the sidebar */
    hideFolder(cwd: string) {
      state.visibleFolders = state.visibleFolders.filter(f => f !== cwd)
      delete state.visibleSessions[cwd]
      notify()
    },

    /** Promote a resumable session to visible in its folder */
    showSession(cwd: string, key: string) {
      const arr = state.visibleSessions[cwd] ??= []
      if (!arr.includes(key)) {
        arr.push(key)
        notify()
      }
    },

    /** Dismiss a session — remove from visible set */
    dismissSession(cwd: string, key: string) {
      const arr = state.visibleSessions[cwd]
      if (arr) {
        state.visibleSessions[cwd] = arr.filter(k => k !== key)
        notify()
      }
    },

    /**
     * Check if a session should be visible (not in "show more").
     * Live sessions are always visible regardless of state.
     */
    isSessionVisible(session: Session): boolean {
      if (session.alive) return true
      const cwd = session.cwd ?? '~'
      const key = session.resume_key ?? session.id
      return state.visibleSessions[cwd]?.includes(key) ?? false
    },

    /** Check if a folder is visible */
    isFolderVisible(cwd: string): boolean {
      return state.visibleFolders.includes(cwd)
    },
  }
}

export type SidebarStateManager = ReturnType<typeof createSidebarState>
