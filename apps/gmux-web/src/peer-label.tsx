/** Colored badge showing a peer's unique prefix. */

import { peerAppearance } from './store'

export function PeerLabel({ name }: { name: string }) {
  const a = peerAppearance.value.get(name)
  return (
    <span
      class="session-peer-label"
      title={name}
      style={a && { color: a.color, background: a.bg }}
    >
      {a?.label ?? name[0].toUpperCase()}
    </span>
  )
}
