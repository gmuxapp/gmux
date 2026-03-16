/**
 * Mock data: one file per session in sessions/.
 * Active with ?mock query param or VITE_MOCK=1.
 */

import { groupByFolder } from '../types'
import type { MockSession } from './types'

import claudeRateLimiting from './sessions/claude-rate-limiting'
import pytestApi from './sessions/pytest-api'
import piRateLimiting from './sessions/pi-rate-limiting'
import piFixAuthBug from './sessions/pi-fix-auth-bug'
import piAdapterSystem from './sessions/pi-adapter-system'
import piFixWebsocket from './sessions/pi-fix-websocket'
import piUpdateDocs from './sessions/pi-update-docs'

/** All mock sessions. First alive session is auto-selected. */
export const MOCK_SESSIONS: MockSession[] = [
  claudeRateLimiting,
  pytestApi,
  piRateLimiting,
  piFixAuthBug,
  piAdapterSystem,
  piFixWebsocket,
  piUpdateDocs,
]

/** Session ID → mock session (for terminal content + cursor lookup). */
export const MOCK_BY_ID: Record<string, MockSession> = Object.fromEntries(
  MOCK_SESSIONS.map(m => [m.id, m]),
)

export function getMockFolders() {
  return groupByFolder(MOCK_SESSIONS)
}
