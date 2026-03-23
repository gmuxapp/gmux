import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const BOLD = '\x1b[1m'
const DIM = '\x1b[2m'
const RED = '\x1b[31m'
const GREEN = '\x1b[32m'
const YELLOW = '\x1b[33m'
const MAGENTA = '\x1b[35m'
const GRAY = '\x1b[90m'
const WHITE = '\x1b[37m'
const BG_GREEN = '\x1b[42m'

export default {
  id: 'sess-d4e5f6',
  created_at: ago(31),
  command: ['pi'],
  cwd: '/home/user/dev/gmux/.grove/teak',
  workspace_root: '/home/user/dev/gmux',
  kind: 'pi',
  alive: true,
  pid: 12346,
  exit_code: null,
  started_at: ago(31),
  exited_at: null,
  title: 'Fix websocket proxy',
  subtitle: 'waiting for approval',
  status: { label: 'waiting for approval', working: false },
  unread: true,
  socket_path: '/tmp/gmux-sessions/mock.sock',
  stale: true,
  terminal: [
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
  ].join('\n'),
} satisfies MockSession
