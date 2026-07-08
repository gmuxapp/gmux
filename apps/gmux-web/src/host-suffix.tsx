/** Textual " · host" suffix for project headers.
 *
 * Renders the peer's name in muted body text after a project name
 * on the sidebar folder header, home project card, and project page
 * h2. Turns the disconnect color when the peer is unreachable.
 *
 * Locally-owned projects (peer omitted) render nothing: the viewer
 * is on that host, so naming it is noise. The suffix exists to flag
 * cross-host ownership; absence of the suffix means "here".
 *
 * `local` marks the name as the viewer's own host (used when a
 * multi-host setup labels local project headers too). The local host
 * is always reachable and is never in `peerStatusByName`, so we skip
 * the peer-status lookup entirely — otherwise a remote peer that
 * happens to share the local hostname would tint local headers red.
 */

import { peerStatusByName } from './store'

export function HostSuffix({ peer, leading = true, local = false, connective }: { peer?: string; leading?: boolean; local?: boolean; connective?: string }) {
  if (!peer) return null

  // 'connected' or unknown (undefined) reads as normal; anything
  // else ('connecting', 'disconnected', 'offline') reads as red.
  // Treating unknown as connected avoids a brief offline flash on
  // first SSE arrival before peers[] populates. The local host is
  // always "us" — never offline, never status-looked-up.
  const status = local ? undefined : peerStatusByName.value.get(peer)
  const offline = !local && status !== undefined && status !== 'connected'

  return (
    <span
      class={`host-suffix${offline ? ' offline' : ''}`}
      title={`${peer}${status ? ` (${status})` : ''}`}
    >
      {connective
        ? <span class="host-suffix-prep">{connective} </span>
        : leading && <span class="host-suffix-sep" aria-hidden="true"> · </span>}{peer}
    </span>
  )
}
