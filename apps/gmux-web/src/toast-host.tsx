/**
 * Fixed-position overlay that renders the live toast stack. Mounted once
 * near the app root. Reads the `toasts` signal directly (auto-subscribed)
 * and never steals focus — it's an aria-live region, not a dialog.
 *
 * Auto-dismiss is driven by the countdown bar's `animationend`: the bar
 * shrinks over the toast's TTL, and when its animation ends the toast is
 * removed. One clock — the visible bar can't disagree with the actual
 * dismissal — and `:hover { animation-play-state: paused }` (in
 * styles.css) pauses both at once for free.
 */

import { toasts, dismissToast, TOAST_TTL_MS } from './toasts'

export function ToastHost() {
  const list = toasts.value
  if (list.length === 0) return null
  return (
    <div class="toast-host" role="region" aria-label="Notifications" aria-live="polite">
      {list.map(t => (
        <div key={t.id} class={`toast toast-${t.kind}`}>
          <div class="toast-body">
            <span class="toast-message">{t.message}</span>
            {t.count > 1 && <span class="toast-count">×{t.count}</span>}
            <button
              class="toast-dismiss"
              title="Dismiss"
              aria-label="Dismiss notification"
              onClick={() => dismissToast(t.id)}
            >×</button>
          </div>
          <div
            class="toast-countdown"
            style={{ animationDuration: `${TOAST_TTL_MS[t.kind]}ms` }}
            onAnimationEnd={() => dismissToast(t.id)}
          />
        </div>
      ))}
    </div>
  )
}
