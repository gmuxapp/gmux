import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const BOLD = '\x1b[1m'
const DIM = '\x1b[2m'
const RED = '\x1b[31m'
const GREEN = '\x1b[32m'
const CYAN = '\x1b[36m'
const MAGENTA = '\x1b[35m'
const GRAY = '\x1b[90m'
const WHITE = '\x1b[37m'
const BG_GREEN = '\x1b[42m'
const BG_RED = '\x1b[41m'

export default {
  id: 'sess-j1k2l3',
  created_at: ago(15),
  command: ['pi'],
  cwd: '/home/user/dev/gmux',
  workspace_root: '/home/user/dev/gmux',
  kind: 'pi',
  alive: false,
  pid: 23456,
  exit_code: null,
  started_at: ago(15),
  exited_at: null,
  title: 'fix auth bug',
  subtitle: 'running tests',
  status: { label: 'running tests', working: true },
  unread: false,
  socket_path: '/tmp/gmux-sessions/mock.sock',
  terminal: [
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
  ].join('\n'),
} satisfies MockSession
