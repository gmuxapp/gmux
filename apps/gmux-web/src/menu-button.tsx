import { unreadCount } from './store'
import { useArrivalPulse } from './use-arrival-pulse'

/**
 * The sidebar hamburger with the waiting-dot badge. One component, three
 * touch-only placements (`variant` picks the styling):
 *
 * - `bar`: a cell in the mobile key bar (alive sessions).
 * - `footer`: in ReplayView's action bar (dead sessions, which render no
 *   key bar — showing a full set of dead keys just to carry ☰ read as
 *   clutter).
 * - `floating`: bottom-left overlay on the home screen,
 *   which have no bottom bar at all.
 *
 * The badge surfaces only the waiting (unread) state — working/active are
 * deliberately omitted. unreadCount excludes the selected session and its
 * value re-fires the arrival pulse when another session starts waiting.
 *
 * All variants are hidden on fine pointers via CSS; on desktop the sidebar
 * is always visible, so the button would be dead weight.
 */
export function MenuButton({ variant, onMenu }: {
  variant: 'bar' | 'footer' | 'floating'
  onMenu: () => void
}) {
  const waitingCount = unreadCount.value
  const waiting = waitingCount > 0
  const arrival = useArrivalPulse(waiting ? 'unread' : 'none', waitingCount)

  const variantClass = variant === 'bar'
    ? 'mk-menu'
    : `menu-btn-standalone${variant === 'floating' ? ' menu-btn-floating' : ''}`

  return (
    <button
      class={`mobile-bottom-action menu-btn ${variantClass}${waiting ? ' bg-waiting' : ''}${arrival ? ` bg-${arrival}` : ''}`}
      onClick={() => {
        // Dismiss the on-screen keyboard when opening the menu. The key
        // bar holds focus on the textarea through pointerdown (keepFocus),
        // so it's still the active element here; blurring it slides the
        // keyboard away. Harmless in the standalone placements.
        (document.activeElement as HTMLElement | null)?.blur()
        onMenu()
      }}
      title="Open sessions"
    ><span class="mkey-face">☰</span></button>
  )
}
