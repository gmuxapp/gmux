/**
 * log-sink.ts — dev-only console interceptor.
 *
 * Monkey-patches console.error/warn/debug/log to also POST to the local
 * gmux-logd server (127.0.0.1:9876). If logd isn't running the fetch fails
 * silently — no impact on the app.
 *
 * Loaded once from main.tsx, only in development builds.
 */

const LOGD = 'http://127.0.0.1:9876/log'

type Level = 'error' | 'warn' | 'info' | 'debug'

function send(level: Level, args: unknown[]) {
  // Stringify args the same way the browser console would display them.
  const parts = args.map(a => {
    if (typeof a === 'string') return a
    try { return JSON.stringify(a) } catch { return String(a) }
  })
  const msg = parts[0] as string ?? ''
  const data = parts.slice(1).join(' ')

  fetch(LOGD, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ source: 'frontend', level, msg, data }),
  }).catch(() => {})
}

const orig = {
  error: console.error.bind(console),
  warn:  console.warn.bind(console),
  debug: console.debug.bind(console),
  log:   console.log.bind(console),
}

console.error = (...args: unknown[]) => { orig.error(...args); send('error', args) }
console.warn  = (...args: unknown[]) => { orig.warn(...args);  send('warn',  args) }
console.debug = (...args: unknown[]) => { orig.debug(...args); send('debug', args) }
// console.log intentionally skipped — too noisy

export {}
