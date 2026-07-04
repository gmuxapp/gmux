export type WsState = 'connecting' | 'open' | 'lost'

/**
 * State transition for a WebSocket close event.
 *
 * A terminal may briefly have two sockets alive: when the connection effect
 * re-runs (session switch, font load, callback identity change) or when
 * connect() replaces a lingering socket, the old socket's close event
 * dispatches asynchronously — often *after* the replacement socket has
 * already opened. If that stale close were allowed to transition
 * 'open' → 'lost', the "Connection lost, reconnecting…" pill would get stuck
 * on screen while the live socket streams output behind it, because nothing
 * else ever clears it.
 *
 * Only the socket that is still the current one may mark the connection lost.
 */
export function wsStateOnClose(prev: WsState, isCurrentSocket: boolean): WsState {
  if (!isCurrentSocket) return prev
  return prev === 'open' ? 'lost' : prev
}
