// PiSessionView — rendered for pi-sdk and pi-sdk-sbx sessions.
// Step 4: routing stub. Step 5 will wire the WebSocket and render events.
import { type Session } from './types'

interface PiSessionViewProps {
  session: Session
  isActive: boolean
}

export function PiSessionView({ session, isActive }: PiSessionViewProps) {
  return (
    <div
      class="pi-session-view"
      style={{ display: isActive ? 'flex' : 'none', flexDirection: 'column', flex: 1, alignItems: 'center', justifyContent: 'center', color: 'var(--text-dim)' }}
    >
      <div style={{ fontSize: 13, fontFamily: 'monospace' }}>
        pi-sdk session — {session.id.slice(0, 8)}
        {session.subtitle ? ` (${session.subtitle})` : ''}
      </div>
    </div>
  )
}

/** Returns true for sessions driven by the pi-sdk subprocess adapter. */
export function isPiSDKSession(session: { kind: string }): boolean {
  return session.kind === 'pi-sdk' || session.kind === 'pi-sdk-sbx'
}
