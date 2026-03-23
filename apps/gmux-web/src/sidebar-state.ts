/**
 * Sidebar folder visibility — stored in localStorage.
 *
 * Tracks which folder paths are visible in the sidebar. Paths are keyed
 * by workspace root (when sessions share a VCS root) or cwd otherwise.
 * New folders auto-show when they have live sessions.
 */

import type { Session } from './types'

const STORAGE_KEY = 'gmux-sidebar-state'

interface PersistedState {
  visibleFolders: string[]
}

function load(): PersistedState {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw) {
      const parsed = JSON.parse(raw)
      // Accept both old format (with visibleSessions) and new.
      return { visibleFolders: parsed.visibleFolders ?? [] }
    }
  } catch { /* corrupt or missing — start fresh */ }
  return { visibleFolders: [] }
}

function save(state: PersistedState) {
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
    subscribe(fn: () => void) {
      listeners.add(fn)
      return () => { listeners.delete(fn) }
    },

    /** Auto-show folders that have live sessions. */
    syncSessions(sessions: Session[]) {
      let changed = false
      for (const s of sessions) {
        if (!s.alive) continue
        // Track by the grouping key: workspace root if set, otherwise cwd.
        const key = s.workspace_root || s.cwd || '~'
        if (!state.visibleFolders.includes(key)) {
          state.visibleFolders.push(key)
          changed = true
        }
      }
      if (changed) notify()
    },

    showFolder(cwd: string) {
      if (!state.visibleFolders.includes(cwd)) {
        state.visibleFolders.push(cwd)
        notify()
      }
    },

    hideFolder(cwd: string) {
      state.visibleFolders = state.visibleFolders.filter(f => f !== cwd)
      notify()
    },

    isFolderVisible(cwd: string): boolean {
      return state.visibleFolders.includes(cwd)
    },
  }
}

export type SidebarStateManager = ReturnType<typeof createSidebarState>
