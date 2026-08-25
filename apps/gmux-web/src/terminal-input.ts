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
): boolean {
  return inputClaimed
    && currentConnection !== null
    && currentConnection === expectedConnection
    && currentConnection.sessionId === currentSessionId
    && currentConnection.ws.readyState === 1 // WebSocket.OPEN
}
