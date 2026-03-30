/**
 * Sidebar folder visibility — stored in localStorage.
 *
 * Tracks which folder paths are visible in the sidebar. Paths are keyed
 * by the same grouping used in groupByFolder: the most common remote URL
 * when remotes are present, workspace root, or cwd as fallback.
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
        const key = sessionGroupKey(s)
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

/**
 * Derive the grouping key for a single session. Used to track folder
 * visibility independently of the full groupByFolder union-find.
 * Returns the first remote URL (origin preferred), workspace root, or cwd.
 */
function sessionGroupKey(s: Session): string {
  if (s.remotes) {
    // Prefer origin, fall back to first available.
    if (s.remotes.origin) return s.remotes.origin
    const values = Object.values(s.remotes)
    if (values.length > 0) return values[0]
  }
  return s.workspace_root || s.cwd || '~'
}
