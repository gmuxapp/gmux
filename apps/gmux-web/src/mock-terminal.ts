/**
 * Pre-baked ANSI terminal content for mock mode.
 * Each snapshot is raw escape-sequence data that gets written into an xterm.js
 * instance to produce a realistic-looking terminal "screenshot".
 *
 * Keyed by session ID so different mock sessions show different content.
 */

// Helper: produce a line with color via SGR codes.
const RST = '\x1b[0m'
const BOLD = '\x1b[1m'
const DIM = '\x1b[2m'
const GREEN = '\x1b[32m'
const YELLOW = '\x1b[33m'
const BLUE = '\x1b[34m'
const MAGENTA = '\x1b[35m'
const CYAN = '\x1b[36m'
const RED = '\x1b[31m'
const GRAY = '\x1b[90m'
const WHITE = '\x1b[37m'
const BG_GREEN = '\x1b[42m'
const BG_RED = '\x1b[41m'
const BG_YELLOW = '\x1b[43m'
const BG_BLUE = '\x1b[44m'

// ── Pi session: "implement adapter system" ──
const piThinking = [
  `${GRAY}╭──────────────────────────────────────────────────────╮${RST}`,
  `${GRAY}│${RST} ${BOLD}${MAGENTA}●${RST} ${BOLD}pi${RST} ${DIM}— implement adapter system${RST}${GRAY}                       │${RST}`,
  `${GRAY}╰──────────────────────────────────────────────────────╯${RST}`,
  ``,
  `${BOLD}${CYAN}⠋ Thinking…${RST}`,
  ``,
  `I'll implement the adapter system with a plugin interface that lets`,
  `different AI backends register themselves. Let me start with the`,
  `core types:`,
  ``,
  `${DIM}${GRAY}src/adapters/types.ts${RST}`,
  `${GREEN}+${RST} export interface Adapter {`,
  `${GREEN}+${RST}   readonly name: string`,
  `${GREEN}+${RST}   readonly version: string`,
  `${GREEN}+${RST}   connect(opts: ConnectOptions): Promise<Session>`,
  `${GREEN}+${RST}   disconnect(): Promise<void>`,
  `${GREEN}+${RST} }`,
  `${GREEN}+${RST}`,
  `${GREEN}+${RST} export interface ConnectOptions {`,
  `${GREEN}+${RST}   model: string`,
  `${GREEN}+${RST}   temperature?: number`,
  `${GREEN}+${RST}   maxTokens?: number`,
  `${GREEN}+${RST} }`,
  ``,
  `Now the registry:`,
  ``,
  `${DIM}${GRAY}src/adapters/registry.ts${RST}`,
  `${GREEN}+${RST} const adapters = new Map<string, Adapter>()`,
  `${GREEN}+${RST}`,
  `${GREEN}+${RST} export function registerAdapter(adapter: Adapter): void {`,
  `${GREEN}+${RST}   if (adapters.has(adapter.name)) {`,
  `${GREEN}+${RST}     throw new Error(\`Adapter "\${adapter.name}" already registered\`)`,
  `${GREEN}+${RST}   }`,
  `${GREEN}+${RST}   adapters.set(adapter.name, adapter)`,
  `${GREEN}+${RST} }`,
].join('\r\n')

// ── Pi session: "fix websocket proxy" (waiting for input) ──
const piWaiting = [
  `${GRAY}╭──────────────────────────────────────────────────────╮${RST}`,
  `${GRAY}│${RST} ${BOLD}${MAGENTA}●${RST} ${BOLD}pi${RST} ${DIM}— fix websocket proxy${RST}${GRAY}                              │${RST}`,
  `${GRAY}╰──────────────────────────────────────────────────────╯${RST}`,
  ``,
  `I found the issue — the WebSocket proxy wasn't forwarding close`,
  `frames correctly. Here's the fix:`,
  ``,
  `${DIM}${GRAY}services/gmuxd/internal/wsproxy/wsproxy.go${RST}`,
  `${RED}-${RST}     ws.Close()`,
  `${GREEN}+${RST}     ws.Close(websocket.StatusNormalClosure, "session ended")`,
  ``,
  `${DIM}${GRAY}Ran 28 tests — all passing.${RST}`,
  ``,
  `${BOLD}${BG_GREEN}${WHITE} PASS ${RST} ${GREEN}28/28 tests passed${RST} ${DIM}(2.3s)${RST}`,
  ``,
  `Should I also add a test for the close frame handling? This edge`,
  `case wasn't covered before.`,
  ``,
  `${YELLOW}▎${RST} ${BOLD}Waiting for your response…${RST}`,
  `${GRAY}  Type your reply and press Enter${RST}`,
].join('\r\n')

// ── Pi session: "setup monorepo" (completed) ──
const piCompleted = [
  `${GRAY}╭──────────────────────────────────────────────────────╮${RST}`,
  `${GRAY}│${RST} ${BOLD}${MAGENTA}●${RST} ${BOLD}pi${RST} ${DIM}— setup monorepo${RST}${GRAY}                                  │${RST}`,
  `${GRAY}╰──────────────────────────────────────────────────────╯${RST}`,
  ``,
  `${GREEN}✓${RST} Created workspace root with turborepo`,
  `${GREEN}✓${RST} Configured pnpm workspaces`,
  `${GREEN}✓${RST} Set up shared tsconfig`,
  `${GREEN}✓${RST} Added packages/protocol with zod schemas`,
  `${GREEN}✓${RST} Added apps/gmux-web with vite + preact`,
  `${GREEN}✓${RST} Added apps/gmux-api with trpc`,
  `${GREEN}✓${RST} All packages building and tests passing`,
  ``,
  `${BOLD}${BG_GREEN}${WHITE} PASS ${RST} ${GREEN}All tasks completed${RST}`,
  ``,
  `${DIM}The monorepo is set up with the following structure:${RST}`,
  ``,
  `  ${BLUE}gmux/${RST}`,
  `  ├── ${CYAN}apps/${RST}`,
  `  │   ├── ${WHITE}gmux-web${RST}  ${DIM}(preact + vite)${RST}`,
  `  │   └── ${WHITE}gmux-api${RST}  ${DIM}(trpc + hono)${RST}`,
  `  ├── ${CYAN}packages/${RST}`,
  `  │   └── ${WHITE}protocol${RST}  ${DIM}(shared zod schemas)${RST}`,
  `  ├── ${CYAN}services/${RST}`,
  `  │   └── ${WHITE}gmuxd${RST}     ${DIM}(Go daemon)${RST}`,
  `  └── ${DIM}turbo.json${RST}`,
  ``,
  `${DIM}Session ended with exit code 0${RST}`,
].join('\r\n')

// ── Pi session: "fix auth bug" (running tests) ──
const piTesting = [
  `${GRAY}╭──────────────────────────────────────────────────────╮${RST}`,
  `${GRAY}│${RST} ${BOLD}${MAGENTA}●${RST} ${BOLD}pi${RST} ${DIM}— fix auth bug${RST}${GRAY}                                    │${RST}`,
  `${GRAY}╰──────────────────────────────────────────────────────╯${RST}`,
  ``,
  `Found the issue — the JWT validation was using the wrong clock`,
  `skew tolerance. Fixing now:`,
  ``,
  `${DIM}${GRAY}src/auth/jwt.ts${RST}`,
  `${RED}-${RST}   clockTolerance: 0,`,
  `${GREEN}+${RST}   clockTolerance: 30, // 30 seconds`,
  ``,
  `Running the test suite to verify…`,
  ``,
  `  ${BOLD}${WHITE}RUNS${RST}  src/auth/__tests__/jwt.test.ts`,
  `  ${BG_GREEN}${WHITE} PASS ${RST}  src/auth/__tests__/validate.test.ts  ${DIM}(0.8s)${RST}`,
  `  ${BG_GREEN}${WHITE} PASS ${RST}  src/auth/__tests__/refresh.test.ts   ${DIM}(1.2s)${RST}`,
  `  ${BG_GREEN}${WHITE} PASS ${RST}  src/middleware/__tests__/auth.test.ts ${DIM}(0.5s)${RST}`,
  `  ${BG_RED}${WHITE} FAIL ${RST}  src/auth/__tests__/session.test.ts   ${DIM}(1.1s)${RST}`,
  ``,
  `  ${RED}● session management › should expire after TTL${RST}`,
  ``,
  `    ${RED}Expected: 401${RST}`,
  `    ${GREEN}Received: 200${RST}`,
  ``,
  `  ${BOLD}${CYAN}⠋ Investigating the failing test…${RST}`,
].join('\r\n')

// ── Pi session: "refactor models" (failed) ──
const piFailed = [
  `${GRAY}╭──────────────────────────────────────────────────────╮${RST}`,
  `${GRAY}│${RST} ${BOLD}${MAGENTA}●${RST} ${BOLD}pi${RST} ${DIM}— refactor models${RST}${GRAY}                                 │${RST}`,
  `${GRAY}╰──────────────────────────────────────────────────────╯${RST}`,
  ``,
  `Refactoring the model layer to use the repository pattern…`,
  ``,
  `${GREEN}✓${RST} Created base Repository<T> class`,
  `${GREEN}✓${RST} Migrated UserRepository`,
  `${GREEN}✓${RST} Migrated PostRepository`,
  `${RED}✗${RST} Failed to migrate CommentRepository`,
  ``,
  `${BOLD}${BG_RED}${WHITE} ERROR ${RST} ${RED}Circular dependency detected${RST}`,
  ``,
  `  ${DIM}CommentRepository → PostRepository → CommentRepository${RST}`,
  ``,
  `  ${RED}This needs a design change to break the cycle. The current`,
  `  ${RED}approach of injecting repositories into each other won't work.${RST}`,
  ``,
  `${DIM}Session ended with exit code 1${RST}`,
].join('\r\n')

// ── Pi session: "update API reference" ──
const piIdle = [
  `${GRAY}╭──────────────────────────────────────────────────────╮${RST}`,
  `${GRAY}│${RST} ${BOLD}${MAGENTA}●${RST} ${BOLD}pi${RST} ${DIM}— update API reference${RST}${GRAY}                             │${RST}`,
  `${GRAY}╰──────────────────────────────────────────────────────╯${RST}`,
  ``,
  `${GREEN}✓${RST} Updated REST endpoint documentation`,
  `${GREEN}✓${RST} Added WebSocket protocol reference`,
  `${GREEN}✓${RST} Generated OpenAPI spec`,
  ``,
  `The API docs are up to date. Here's what changed:`,
  ``,
  `  ${CYAN}docs/api/${RST}`,
  `  ├── ${GREEN}+${RST} ${WHITE}websocket.md${RST}     ${DIM}(new)${RST}`,
  `  ├── ${YELLOW}~${RST} ${WHITE}rest.md${RST}          ${DIM}(updated 12 endpoints)${RST}`,
  `  └── ${YELLOW}~${RST} ${WHITE}openapi.yaml${RST}     ${DIM}(regenerated)${RST}`,
  ``,
  `${DIM}Anything else you'd like me to update?${RST}`,
].join('\r\n')

// ── Generic session: "pytest" ──
const pytest = [
  `${BOLD}${WHITE}$ pytest tests/ -v${RST}`,
  `${DIM}================================ test session starts =================================${RST}`,
  `${DIM}platform linux -- Python 3.12.3, pytest-8.1.1${RST}`,
  `${DIM}collected 50 items${RST}`,
  ``,
  `tests/test_api.py::test_health_check ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_api.py::test_create_user ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_api.py::test_get_user ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_api.py::test_update_user ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_api.py::test_delete_user ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_api.py::test_list_users ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_auth.py::test_login ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_auth.py::test_logout ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_auth.py::test_refresh_token ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_auth.py::test_invalid_token ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_auth.py::test_expired_token ${BG_RED}${WHITE} FAILED ${RST}`,
  `tests/test_models.py::test_user_model ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_models.py::test_post_model ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_models.py::test_comment_model ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_models.py::test_relationships ${BG_GREEN}${WHITE} PASSED ${RST}`,
  `tests/test_rate_limit.py::test_rate_limit_basic ${BOLD}${WHITE}RUNNING${RST}`,
].join('\r\n')

// ── Pi session: "add rate limiting" ──
const piRateLimiting = [
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
].join('\r\n')

// ── Pi session: "terraform migration" (old, completed) ──
const piTerraform = [
  `${GRAY}╭──────────────────────────────────────────────────────╮${RST}`,
  `${GRAY}│${RST} ${BOLD}${MAGENTA}●${RST} ${BOLD}pi${RST} ${DIM}— terraform migration${RST}${GRAY}                              │${RST}`,
  `${GRAY}╰──────────────────────────────────────────────────────╯${RST}`,
  ``,
  `${GREEN}✓${RST} Migrated from Terraform 0.14 to OpenTofu 1.6`,
  `${GREEN}✓${RST} Updated provider lock files`,
  `${GREEN}✓${RST} Ran plan — no resource changes`,
  `${GREEN}✓${RST} Applied successfully`,
  ``,
  `${BOLD}${BG_GREEN}${WHITE} PASS ${RST} ${GREEN}Migration complete — zero downtime${RST}`,
  ``,
  `${DIM}Session ended with exit code 0${RST}`,
].join('\r\n')


/** Map of session ID → ANSI content to write into mock xterm instances. */
export const MOCK_TERMINAL_CONTENT: Record<string, string> = {
  'sess-a1b2c3': piThinking,
  'sess-d4e5f6': piWaiting,
  'sess-g7h8i9': piCompleted,
  'sess-j1k2l3': piTesting,
  'sess-m4n5o6': piFailed,
  'sess-p7q8r9': piIdle,
  'sess-s1t2u3': pytest,
  'sess-v4w5x6': piRateLimiting,
  'sess-y7z8a9': piTerraform,
}

/**
 * Map of session ID → screenshot image path (relative to public/).
 * Sessions with a screenshot show the image instead of an xterm instance.
 * Place screenshots in public/screenshots/ and reference them here.
 *
 * Example:
 *   'sess-a1b2c3': '/screenshots/adapter-system.png',
 */
export const MOCK_SCREENSHOTS: Record<string, string> = {
  // Add captured screenshots here:
  // 'sess-a1b2c3': '/screenshots/adapter-system.png',
}
