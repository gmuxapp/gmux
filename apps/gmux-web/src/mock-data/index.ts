/**
 * Mock data: one file per session in sessions/.
 * Active with ?mock query param or VITE_MOCK=1.
 */

import type { ProjectItem, PeerInfo } from '../types'
import type { HealthData } from '../store'
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

/** Mock peers (host roster). Covers all three Hosts-tab groups
 *  (tailnet-discovered, devcontainer, manual), a connected and a
 *  disconnected row, and a manual host carrying a last_error so the
 *  grouping and every row state are exercised. */
export const MOCK_PEERS: PeerInfo[] = [
  { name: 'laptop', url: 'https://laptop.tail-scale.ts.net', status: 'connected', session_count: 2, version: '1.2.0', source: 'tailscale' },
  { name: 'server', url: 'https://server.tail-scale.ts.net', status: 'connected', session_count: 4, version: '1.1.9', source: 'tailscale' },
  { name: 'devcontainer', url: 'https://devcontainer.tail-scale.ts.net', status: 'connected', session_count: 1, version: '1.2.0', local: true, source: 'devcontainer' },
  { name: 'konyvtar', url: 'https://gmux.tail95157a.ts.net', status: 'connected', session_count: 1, version: '1.2.0', source: 'manual' },
  { name: 'bespin', url: 'https://bespin.tail-scale.ts.net', status: 'disconnected', session_count: 0, last_error: 'dial tcp 100.84.12.9:443: connect: connection refused', source: 'manual' },
]

/** Mock daemon health for the local host. */
export const MOCK_HEALTH: HealthData = {
  version: '1.2.0',
  hostname: 'workstation',
  peers: MOCK_PEERS,
}

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
  // A resolved reference to a roster peer (server).
  { slug: 'api', peer: 'server' },
  // An unresolved reference: 'old-tower' is in no roster bucket, so it
  // surfaces as a host-not-found state in the sidebar and Hosts tab.
  { slug: 'legacy-app', peer: 'old-tower' },
]
