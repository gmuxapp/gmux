import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const BOLD = '\x1b[1m'
const DIM = '\x1b[2m'
const GREEN = '\x1b[32m'
const MAGENTA = '\x1b[35m'
const GRAY = '\x1b[90m'

export default {
  id: 'sess-pi-scroll',
  created_at: ago(5),
  command: ['pi'],
  cwd: '/home/user/dev/my-project',
  workspace_root: '/home/user/dev/my-project',
  remotes: { origin: 'github.com/acme/my-project' },
  kind: 'pi',
  alive: true,
  pid: 44300,
  exit_code: null,
  started_at: ago(5),
  exited_at: null,
  title: 'fix scrollback',
  subtitle: '',
  status: { label: '', working: false },
  unread: false,
  socket_path: '/tmp/gmux-sessions/mock.sock',
  peer: 'devcontainer',
  terminal: [
    `${GRAY}╭──────────────────────────────────────────────────────╮${RST}`,
    `${GRAY}│${RST} ${BOLD}${MAGENTA}●${RST} ${BOLD}pi${RST} ${DIM}— fix scrollback${RST}${GRAY}                                  │${RST}`,
    `${GRAY}╰──────────────────────────────────────────────────────╯${RST}`,
    ``,
    `Found the scrollback buffer overflow. The ring buffer wasn't`,
    `wrapping correctly when the terminal exceeded 10k lines.`,
    ``,
    `${DIM}${GRAY}src/terminal/scrollback.ts${RST}`,
    `${GREEN}+${RST} if (this.offset >= this.capacity) {`,
    `${GREEN}+${RST}   this.offset = 0`,
    `${GREEN}+${RST} }`,
  ].join('\n'),
} satisfies MockSession
