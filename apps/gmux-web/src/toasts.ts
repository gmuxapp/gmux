/**
 * Transient notification ("toast") layer.
 *
 * Backend/action failures used to be `console.warn`'d and swallowed, so
 * a rejected Resume/Kill/Launch looked like nothing happened. This module
 * holds the toast state (a single signal) plus the pure list operations
 * behind it, so the push/dismiss/coalesce/cap logic is unit-testable
 * without a DOM.
 *
 * Auto-dismiss is driven by the CSS countdown-bar animation's
 * `animationend` (see toast-host.tsx), not a JS timer — one clock, so
 * the visible bar can never disagree with when the toast actually
 * disappears, and pause-on-hover comes free via `animation-play-state`.
 *
 * State only; the visual overlay lives in `<ToastHost />` (toast-host.tsx).
 */

import { signal } from '@preact/signals'

export type ToastKind = 'error' | 'info'

export interface Toast {
  id: string
  kind: ToastKind
  message: string
  /** Wall-clock ms when (re)pushed. */
  at: number
  /** How many times this exact message has fired; rendered as "(×N)"
   *  when >1. Coalescing a duplicate bumps this instead of stacking. */
  count: number
}

/** Live toast list, newest last. Rendered by `<ToastHost />`. */
export const toasts = signal<Toast[]>([])

/** Auto-dismiss delay (ms), used to set the countdown-bar animation
 *  duration. Errors linger longer than info so a failure message isn't
 *  gone before it's read. */
export const TOAST_TTL_MS = { error: 6_000, info: 4_000 } as const

/** Hard cap so a flood (e.g. a retry storm of distinct messages) can't
 *  bury the screen. Oldest toasts past the cap are dropped. */
export const MAX_TOASTS = 4

let _seq = 0

// ── Pure list ops (tested directly) ─────────────────────────────────────────

/**
 * Append `toast`, or — if an entry with the same kind+message is already
 * showing — coalesce into it: bump that entry's count, refresh its
 * timestamp, and move it to the end (so its countdown bar restarts and
 * it reads as the most recent). Otherwise append and enforce
 * `MAX_TOASTS` by dropping the oldest. Pure.
 */
export function addToast(list: Toast[], toast: Toast): Toast[] {
  const existing = list.find(t => t.kind === toast.kind && t.message === toast.message)
  if (existing) {
    const coalesced: Toast = {
      ...existing,
      count: existing.count + 1,
      at: toast.at,
      // Reuse the new id so the host treats it as a fresh element and
      // restarts the countdown-bar animation from full.
      id: toast.id,
    }
    return [...list.filter(t => t !== existing), coalesced]
  }
  const next = [...list, toast]
  return next.length > MAX_TOASTS ? next.slice(next.length - MAX_TOASTS) : next
}

/** Remove the toast with `id`. Pure. */
export function removeToast(list: Toast[], id: string): Toast[] {
  return list.filter(t => t.id !== id)
}

// ── Signal-backed API (used by the app) ──────────────────────────────────────

/** Dismiss a toast now (manual close or countdown elapsed). Idempotent. */
export function dismissToast(id: string) {
  toasts.value = removeToast(toasts.value, id)
}

/**
 * Push a toast. Returns the (possibly coalesced) entry's id. Dismissal
 * is scheduled by `<ToastHost />` off the countdown-bar `animationend`,
 * not here — keeping the timer and the visible bar as a single clock.
 */
export function pushToast(kind: ToastKind, message: string): string {
  const id = `t${++_seq}`
  toasts.value = addToast(toasts.value, { id, kind, message, at: Date.now(), count: 1 })
  return id
}

/** Convenience: error toast. */
export function pushError(message: string): string {
  return pushToast('error', message)
}
