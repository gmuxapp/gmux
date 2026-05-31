/** Colored badge showing a peer's unique prefix.
 *
 * Status-aware: reads peerStatusByName and dims when the peer is
 * disconnected. Callers don't need to thread peer status through
 * because connection state is intrinsic to the chip's meaning. */

import { peerAppearance, peerStatusByName } from './store'

export function PeerLabel({ name }: { name: string }) {
  const a = peerAppearance.value.get(name)
  const status = peerStatusByName.value.get(name)
  const connected = status === 'connected'
  const cls = `flex-shrink-0 self-center inline-flex items-center justify-center w-3.5 h-3.5 rounded-[3px] text-[9px] font-semibold leading-[1cap] bg-[oklch(25%_0.04_250)] text-[oklch(72%_0.08_250)]${connected ? '' : ' opacity-40 line-through'}`
  return (
    <span
      class={cls}
      title={connected ? name : `${name} (${status ?? 'unknown'})`}
      style={a && { color: a.color, background: a.bg }}
    >
      {a?.label ?? name[0].toUpperCase()}
    </span>
  )
}
