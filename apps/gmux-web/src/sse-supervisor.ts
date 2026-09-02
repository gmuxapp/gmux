export interface SSESource {
  readonly readyState: number
  addEventListener(type: string, listener: (event: MessageEvent) => void): void
  close(): void
}

export interface SSESupervisorCallbacks {
  opened: () => void
  activity: () => void
  failed: () => void
}

export interface SSESupervisorOptions {
  connect: (callbacks: SSESupervisorCallbacks) => SSESource
  onFailure?: () => void
  onRetryScheduled?: () => void
  onExhausted?: () => void
  now?: () => number
  random?: () => number
  maxRetryDurationMs?: number
  maxDelayMs?: number
  stableOpenDurationMs?: number
  staleDurationMs?: number
  attemptTimeoutMs?: number
}

export interface SSESupervisor {
  start(): void
  stop(): void
  retry(): void
  revalidate(): void
}

/**
 * Owns EventSource replacement rather than delegating recovery to the
 * browser's native retry. Native retry can terminate permanently after a
 * fatal response (notably 401/503), leaving an EventSource in CLOSED forever.
 *
 * Retries are bounded by elapsed time, cancellable, and jittered. The budget
 * model has three rules:
 *
 *  - Only a source that stayed open at least `stableOpenDurationMs` credits
 *    success. A server that accepts and immediately drops the stream keeps
 *    consuming one window instead of resetting it on every flap.
 *  - When the window runs out the supervisor stops entirely: no source, no
 *    timer, nothing to spin in a background tab. The UI offers a manual retry.
 *  - `revalidate()` (a wake) and `retry()` (a tap) are new information and
 *    always open a fresh window, including after exhaustion — a phone that
 *    returns after ten minutes must recover without a tap. They never stack
 *    sources: a pending retry or an in-flight attempt absorbs the wake. A
 *    wake refreshes the deadline only; a tap also restores the backoff floor.
 *
 * Lifecycle revalidation replaces only an unhealthy or stale source: after
 * mobile or a frozen tab resumes, an OPEN readyState is not proof that the
 * TCP stream still delivers bytes, but a stream that delivered bytes moments
 * ago needs no new (full) bootstrap.
 */
export function createSSESupervisor(options: SSESupervisorOptions): SSESupervisor {
  const now = options.now ?? Date.now
  const random = options.random ?? Math.random
  const maxRetryDurationMs = options.maxRetryDurationMs ?? 60_000
  const maxDelayMs = options.maxDelayMs ?? 8_000
  const stableOpenDurationMs = options.stableOpenDurationMs ?? 10_000
  const staleDurationMs = options.staleDurationMs ?? 60_000
  const attemptTimeoutMs = options.attemptTimeoutMs ?? 10_000

  let source: SSESource | null = null
  let timer: ReturnType<typeof setTimeout> | null = null
  let lastActivityAt: number | null = null
  let openedAt: number | null = null
  let attemptStartedAt = 0
  let stopped = true
  let generation = 0
  let attempts = 0
  let retryStartedAt: number | null = null

  const clearTimer = () => {
    if (timer !== null) clearTimeout(timer)
    timer = null
  }

  const closeSource = () => {
    lastActivityAt = null
    openedAt = null
    const current = source
    source = null
    current?.close()
  }

  const exhausted = () => {
    clearTimer()
    closeSource()
    // The exhausted window is deliberately left recorded. Only explicit new
    // information — `revalidate()` (a wake) or `retry()` (a tap) — clears it
    // and opens a fresh bounded window; nothing here spins on its own.
    options.onExhausted?.()
  }

  const connect = () => {
    if (stopped) return
    clearTimer()
    if (retryStartedAt !== null && now() - retryStartedAt >= maxRetryDurationMs) {
      exhausted()
      return
    }
    const myGeneration = ++generation
    attemptStartedAt = now()
    let next: SSESource | null = null
    let failedSynchronously = false
    next = options.connect({
      opened: () => {
        if (stopped || myGeneration !== generation) return
        lastActivityAt = now()
        openedAt = now()
      },
      activity: () => {
        if (stopped || myGeneration !== generation) return
        lastActivityAt = now()
      },
      failed: () => {
        failedSynchronously = true
        if (stopped || myGeneration !== generation) return
        options.onFailure?.()
        // Success is credited only by a source that stayed open long enough
        // to be worth something. A server that accepts the request and drops
        // it immediately (open/error flapping) must keep consuming the same
        // bounded window instead of resetting it on every flap.
        if (openedAt !== null && now() - openedAt >= stableOpenDurationMs) {
          attempts = 0
          retryStartedAt = null
        }
        closeSource()
        if (retryStartedAt === null) retryStartedAt = now()
        const elapsed = now() - retryStartedAt
        if (elapsed >= maxRetryDurationMs) {
          exhausted()
          return
        }
        const base = Math.min(500 * Math.pow(2, attempts), maxDelayMs)
        attempts++
        // ±20% jitter avoids a set of resumed tabs reconnecting in lockstep.
        const delay = Math.max(0, Math.round(base * (0.8 + random() * 0.4)))
        options.onRetryScheduled?.()
        timer = setTimeout(connect, delay)
      },
    })
    // A test double or an unusual implementation can report failure during
    // construction, before `source` can be assigned. Do not leak that source
    // or let its late assignment defeat the generation cancellation.
    if (failedSynchronously || stopped || myGeneration !== generation) {
      next?.close()
      return
    }
    source = next
  }

  const restart = () => {
    if (stopped) return
    generation++
    clearTimer()
    closeSource()
    options.onRetryScheduled?.()
    timer = setTimeout(connect, 0)
  }

  return {
    start() {
      if (!stopped) return
      stopped = false
      attempts = 0
      retryStartedAt = null
      connect()
    },
    stop() {
      stopped = true
      generation++
      clearTimer()
      closeSource()
    },
    retry() {
      if (stopped) return
      attempts = 0
      retryStartedAt = null
      restart()
    },
    revalidate() {
      if (stopped) return
      // A healthy, recently active source needs no new full bootstrap: a
      // protocol-3 bootstrap re-sends every session row, so lifecycle churn
      // is expensive on a phone.
      if (source?.readyState === 1 && lastActivityAt !== null
          && now() - lastActivityAt < staleDurationMs) return
      // A wake is new information: it always earns a fresh bounded window,
      // including after the previous one was exhausted. It does *not* reset
      // `attempts`: a tab receiving wakes at the debounce ceiling would then
      // reconnect at the 500 ms floor forever. The backoff keeps growing
      // (capped at `maxDelayMs`) and only a user's `retry()` restores the
      // floor, so self-healing stays immediate while the load stays honest.
      retryStartedAt = null
      // But it must not pile up sources. A pending retry or an attempt still
      // in flight already covers this wake; it now carries the new budget.
      if (timer !== null) return
      if (source !== null && source.readyState === 0
          && now() - attemptStartedAt < attemptTimeoutMs) return
      restart()
    },
  }
}
