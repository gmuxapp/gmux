/**
 * True on touch-capable devices — a coarse pointer OR a positive
 * touch-point count, so it also catches touch screens that report a fine
 * pointer.
 *
 * Used to gate behaviours that would pop or assume the on-screen keyboard
 * (auto-focus on session select/mount, focus-on-tap). Deliberately broader
 * than the coarse-pointer-only checks in keyboard.ts / presence.ts, which
 * must match the `@media (pointer: coarse)` query that drives the mobile
 * layout.
 */
export function isTouchDevice(): boolean {
  return window.matchMedia?.('(pointer: coarse)').matches || navigator.maxTouchPoints > 0
}
