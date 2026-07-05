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

/**
 * State transition for output received on the current socket.
 *
 * Live output proves the connection works, so the disconnected pill must
 * never be visible while data is flowing. This is a safety net for any
 * future path that marks the connection 'lost' without a matching clear.
 *
 * Returns `prev` unchanged (same reference) unless it was 'lost', so calling
 * this per message never triggers a re-render.
 */
export function wsStateOnOutput(prev: WsState): WsState {
  return prev === 'lost' ? 'open' : prev
}
