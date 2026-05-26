/** Textual " · host" suffix for project headers.
 *
 * Renders the peer's name in muted body text after a project name
 * on the sidebar folder header, home project card, and project page
 * h2. Turns the disconnect color when the peer is unreachable.
 *
 * Locally-owned projects (peer omitted) render nothing: the viewer
 * is on that host, so naming it is noise. The suffix exists to flag
 * cross-host ownership; absence of the suffix means "here".
 */

import { peerStatusByName } from './store'

export function HostSuffix({ peer, leading = true }: { peer?: string; leading?: boolean }) {
  if (!peer) return null

  // 'connected' or unknown (undefined) reads as normal; anything
  // else ('connecting', 'disconnected', 'offline') reads as red.
  // Treating unknown as connected avoids a brief offline flash on
  // first SSE arrival before peers[] populates.
  const status = peerStatusByName.value.get(peer)
  const offline = status !== undefined && status !== 'connected'

  return (
    <span
      class={`host-suffix${offline ? ' offline' : ''}`}
      title={`${peer}${status ? ` (${status})` : ''}`}
    >
      {leading && <span class="host-suffix-sep" aria-hidden="true"> · </span>}{peer}
    </span>
  )
}
