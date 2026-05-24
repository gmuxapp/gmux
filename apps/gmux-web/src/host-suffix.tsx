/** Textual " · host" suffix for project headers.
 *
 * Replaces the single-letter PeerLabel pill on project-level UI
 * (sidebar folder headers, home project cards, project page h2).
 * The session-row PeerLabel stays where it is until the unified
 * row component lands.
 *
 * Visual: muted body text appended after a project name. Turns
 * the disconnect color when the peer is unreachable. Tooltip
 * shows the resolved status.
 *
 * `peer` carries the folder.peer for peer-owned projects. Omit it
 * for locally-owned projects, where the suffix reads the daemon's
 * hostname and never shows an offline state (the viewer is always
 * reachable from itself).
 */

import { health, peerStatusByName } from './store'

export function HostSuffix({ peer }: { peer?: string }) {
  const name = peer ?? health.value?.hostname
  if (!name) return null

  // Local projects: always show as connected. Peer projects:
  // 'connected' or unknown reads as normal; anything else
  // ('connecting', 'disconnected', 'offline') reads as red.
  // Treating unknown as connected avoids a brief offline flash on
  // first SSE arrival, before peers[] populates.
  const status = peer ? peerStatusByName.value.get(peer) : 'connected'
  const offline = status !== undefined && status !== 'connected'

  return (
    <span
      class={`host-suffix${offline ? ' offline' : ''}`}
      title={peer ? `${peer}${status ? ` (${status})` : ''}` : name}
    >
      <span class="host-suffix-sep" aria-hidden="true"> · </span>{name}
    </span>
  )
}
