import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const BOLD = '\x1b[1m'
const DIM = '\x1b[2m'
const GREEN = '\x1b[32m'
const YELLOW = '\x1b[33m'
const CYAN = '\x1b[36m'
const MAGENTA = '\x1b[35m'
const GRAY = '\x1b[90m'
const WHITE = '\x1b[37m'

export default {
  id: 'sess-p7q8r9',
  created_at: ago(30),
  command: ['pi'],
  cwd: '/home/user/dev/docs',
  workspace_root: '/home/user/dev/docs',
  kind: 'pi',
  alive: true,
  pid: 34567,
  exit_code: null,
  started_at: ago(30),
  exited_at: null,
  title: 'Update API reference',
  subtitle: '',
  status: { label: '', working: false },
  unread: false,
  socket_path: '/tmp/gmux-sessions/mock.sock',
  terminal: [
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
  ].join('\n'),
} satisfies MockSession
