/**
 * Sidebar project state, backed by the gmuxd /v1/projects API.
 *
 * Replaces the old localStorage-based folder visibility tracking.
 * Project state lives server-side and is synced to all clients via
 * SSE `projects-update` events.
 */

import type { ProjectItem, DiscoveredProject } from './types'

export interface ProjectsData {
  configured: ProjectItem[]
  discovered: DiscoveredProject[]
}

export function createSidebarState() {
  let data: ProjectsData = { configured: [], discovered: [] }
  const listeners = new Set<() => void>()

  function notify() {
    for (const fn of listeners) fn()
  }

  async function fetchProjects() {
    try {
      const resp = await fetch('/v1/projects')
      const json = await resp.json()
      if (json.ok && json.data) {
        data = {
          configured: json.data.configured ?? [],
          discovered: json.data.discovered ?? [],
        }
        notify()
      }
    } catch (err) {
      console.warn('Failed to fetch projects:', err)
    }
  }

  async function putProjects(items: ProjectItem[]) {
    try {
      const resp = await fetch('/v1/projects', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ items }),
      })
      if (!resp.ok) console.warn('PUT /v1/projects failed:', resp.status)
      // SSE `projects-update` will trigger a re-fetch.
    } catch (err) {
      console.warn('PUT /v1/projects error:', err)
    }
  }

  return {
    subscribe(fn: () => void) {
      listeners.add(fn)
      return () => { listeners.delete(fn) }
    },

    get configured(): ProjectItem[] { return data.configured },
    get discovered(): DiscoveredProject[] { return data.discovered },

    /** Fetch project state from the server. Call on mount and SSE reconnect. */
    fetchProjects,

    /** Called when SSE receives a `projects-update` event. */
    handleProjectsUpdate() {
      fetchProjects()
    },

    /** Hide a project (keep it configured but remove from sidebar). */
    async hideProject(slug: string) {
      const items = data.configured.map(item =>
        item.slug === slug ? { ...item, hidden: true } : item
      )
      await putProjects(items)
    },

    /** Unhide a project. */
    async showProject(slug: string) {
      const items = data.configured.map(item =>
        item.slug === slug ? { ...item, hidden: false } : item
      )
      await putProjects(items)
    },

    /** Remove a project from the configured list. */
    async removeProject(slug: string) {
      const items = data.configured.filter(item => item.slug !== slug)
      await putProjects(items)
    },

    /** Add a discovered project. */
    async addProject(req: { remote?: string; paths?: string[] }) {
      try {
        const resp = await fetch('/v1/projects/add', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(req),
        })
        if (!resp.ok) console.warn('POST /v1/projects/add failed:', resp.status)
        // SSE `projects-update` will trigger a re-fetch.
      } catch (err) {
        console.warn('POST /v1/projects/add error:', err)
      }
    },

    /** Replace the full project list (for reorder, bulk edits). */
    updateProjects: putProjects,
  }
}

export type SidebarStateManager = ReturnType<typeof createSidebarState>
