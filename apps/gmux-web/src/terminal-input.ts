export interface TerminalInputConnection {
  readonly sessionId: string
  readonly ws: { readonly readyState: number }
}

/** Shared send-time gate for every terminal user-byte capability. */
export function canSendTerminalInput(
  inputClaimed: boolean,
  currentConnection: TerminalInputConnection | null,
  expectedConnection: TerminalInputConnection | null,
  currentSessionId: string,
  // A type predicate, not just a boolean: passing means the expected
  // connection is the current one and the current one is non-null, so
  // callers can use their captured connection without re-checking.
): expectedConnection is TerminalInputConnection {
  return inputClaimed
    && currentConnection !== null
    && currentConnection === expectedConnection
    && currentConnection.sessionId === currentSessionId
    && currentConnection.ws.readyState === 1 // WebSocket.OPEN
}
