/** Display status for a host row, derived from its connection state.
 *
 *  The peering layer reports only connected / connecting / disconnected
 *  plus a categorized error string (e.g. "authentication failed",
 *  "connection refused"). We fold those into the buckets a user actually
 *  reasons about:
 *
 *    - online      — connected and authenticated
 *    - connecting  — handshake in progress
 *    - auth        — reachable but the token is missing or wrong
 *    - offline     — unreachable (refused / timed out / no such host / TLS)
 *
 *  "Reachable but not gmux" is intentionally not its own bucket: at the
 *  transport level a closed port is indistinguishable from a host that's
 *  down, and a wrong server gives no reliable positive signal, so it
 *  falls under offline with the raw error shown as detail.
 */
export type HostStatusKind = 'online' | 'connecting' | 'auth' | 'offline'

export interface HostStatus {
  kind: HostStatusKind
  label: string
  /** The raw categorized error, surfaced under the row when present. */
  detail?: string
}

export function hostStatus(status: string, lastError?: string): HostStatus {
  if (status === 'connected') return { kind: 'online', label: 'Online' }
  if (status === 'connecting') return { kind: 'connecting', label: 'Connecting…' }
  const e = (lastError ?? '').toLowerCase()
  if (e.includes('auth')) {
    return { kind: 'auth', label: 'Auth needed', detail: lastError }
  }
  return { kind: 'offline', label: 'Offline', detail: lastError }
}
