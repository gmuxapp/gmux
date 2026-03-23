import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const DIM = '\x1b[2m'
const GREEN = '\x1b[32m'
const MAGENTA = '\x1b[35m'
const BOLD = '\x1b[1m'
const GRAY = '\x1b[90m'

export default {
  id: 'sess-v4w5x6',
  created_at: ago(60),
  command: ['pi'],
  cwd: '/home/user/dev/api',
  workspace_root: '/home/user/dev/api',
  kind: 'pi',
  alive: true,
  pid: 56789,
  exit_code: null,
  started_at: ago(60),
  exited_at: null,
  title: 'Add rate limiting',
  subtitle: 'reviewing changes',
  status: { label: '', working: false },
  unread: false,
  socket_path: '/tmp/gmux-sessions/mock.sock',
  terminal: [
    `${GRAY}╭──────────────────────────────────────────────────────╮${RST}`,
    `${GRAY}│${RST} ${BOLD}${MAGENTA}●${RST} ${BOLD}pi${RST} ${DIM}— add rate limiting${RST}${GRAY}                               │${RST}`,
    `${GRAY}╰──────────────────────────────────────────────────────╯${RST}`,
    ``,
    `I've implemented token-bucket rate limiting. Let me walk you`,
    `through the changes:`,
    ``,
    `${DIM}${GRAY}src/middleware/ratelimit.ts${RST} ${DIM}(new file, 47 lines)${RST}`,
    ``,
    `${GREEN}+${RST} export function rateLimit(opts: RateLimitOptions) {`,
    `${GREEN}+${RST}   const buckets = new Map<string, TokenBucket>()`,
    `${GREEN}+${RST}   return async (req, res, next) => {`,
    `${GREEN}+${RST}     const key = opts.keyFn(req)`,
    `${GREEN}+${RST}     const bucket = buckets.get(key) ?? new TokenBucket(`,
    `${GREEN}+${RST}       opts.maxTokens,`,
    `${GREEN}+${RST}       opts.refillRate,`,
    `${GREEN}+${RST}     )`,
    `${GREEN}+${RST}     if (!bucket.consume()) {`,
    `${GREEN}+${RST}       return res.status(429).json({`,
    `${GREEN}+${RST}         error: 'Too many requests',`,
    `${GREEN}+${RST}         retryAfter: bucket.nextRefill(),`,
    `${GREEN}+${RST}       })`,
    `${GREEN}+${RST}     }`,
    `${GREEN}+${RST}     next()`,
    `${GREEN}+${RST}   }`,
    `${GREEN}+${RST} }`,
    ``,
    `${DIM}Want me to add Redis-backed distributed rate limiting, or is`,
    `the in-memory version sufficient for now?${RST}`,
  ].join('\n'),
} satisfies MockSession
