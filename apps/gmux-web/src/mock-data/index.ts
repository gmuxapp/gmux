/**
 * Mock data: one file per session in sessions/.
 * Active with ?mock query param or VITE_MOCK=1.
 */

import type { ProjectItem } from '../types'
import type { MockSession } from './types'

import codexRefactorAdapters from './sessions/codex-refactor-adapters'
import claudeDesignLanding from './sessions/claude-design-landing'
import piFixScrollback from './sessions/pi-fix-scrollback'
import piAutoresearch from './sessions/pi-autoresearch'
import codexMigrateConvex from './sessions/codex-migrate'
import shellOpenclawConfigure from './sessions/shell-openclaw-configure'
import shellOpenclawLogs from './sessions/shell-openclaw-logs'

/** All mock sessions. First alive session is auto-selected. */
export const MOCK_SESSIONS: MockSession[] = [
  codexRefactorAdapters,
  piFixScrollback,
  claudeDesignLanding,
  piAutoresearch,
  codexMigrateConvex,
  shellOpenclawConfigure,
  shellOpenclawLogs,
]

/** Session ID → mock session (for terminal content + cursor lookup). */
export const MOCK_BY_ID: Record<string, MockSession> = Object.fromEntries(
  MOCK_SESSIONS.map(m => [m.id, m]),
)

/**
 * Mock projects, matching the session remotes/paths.
 * These feed into buildProjectFolders the same way
 * the server-side project config does.
 */
export const MOCK_PROJECTS: ProjectItem[] = [
  {
    slug: 'my-project',
    match: [
      { remote: 'github.com/acme/my-project' },
      { path: '~/dev/my-project' },
    ],
  },
  {
    slug: 'openclaw',
    match: [
      { remote: 'github.com/acme/openclaw' },
      { path: '~/dev/openclaw' },
    ],
  },
]
