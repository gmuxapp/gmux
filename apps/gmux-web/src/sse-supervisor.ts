export interface SSESource {
  readonly readyState: number
  addEventListener(type: string, listener: (event: MessageEvent) => void): void
  close(): void
}

export interface SSESupervisorCallbacks {
  opened: () => void
  failed: () => void
}

export interface SSESupervisorOptions {
  connect: (callbacks: SSESupervisorCallbacks) => SSESource
  onRetryScheduled?: () => void
  onExhausted?: () => void
  now?: () => number
  random?: () => number
  maxRetryDurationMs?: number
  maxDelayMs?: number
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
 * Retries are bounded by elapsed time, cancellable, and jittered. A lifecycle
 * revalidation deliberately replaces even an OPEN source: after mobile or a
 * frozen tab resumes, an OPEN readyState does not prove the TCP stream still
 * delivers bytes.
 */
export function createSSESupervisor(options: SSESupervisorOptions): SSESupervisor {
  const now = options.now ?? Date.now
  const random = options.random ?? Math.random
  const maxRetryDurationMs = options.maxRetryDurationMs ?? 60_000
  const maxDelayMs = options.maxDelayMs ?? 8_000

  let source: SSESource | null = null
  let timer: ReturnType<typeof setTimeout> | null = null
  let stopped = true
  let generation = 0
  let attempts = 0
  let retryStartedAt: number | null = null

  const clearTimer = () => {
    if (timer !== null) clearTimeout(timer)
    timer = null
  }

  const closeSource = () => {
    const current = source
    source = null
    current?.close()
  }

  const exhausted = () => {
    clearTimer()
    closeSource()
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
    let next: SSESource | null = null
    next = options.connect({
      opened: () => {
        if (stopped || myGeneration !== generation) return
        attempts = 0
        retryStartedAt = null
      },
      failed: () => {
        if (stopped || myGeneration !== generation) return
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
    source = next
  }

  const restart = () => {
    if (stopped) return
    generation++
    clearTimer()
    closeSource()
    if (retryStartedAt === null) retryStartedAt = now()
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
      // CLOSED and CONNECTING both need an explicit replacement. OPEN is
      // replaced too: readyState alone cannot expose a dead post-resume TCP.
      restart()
    },
  }
}
