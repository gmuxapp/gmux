import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const BOLD = '\x1b[1m'
const DIM = '\x1b[2m'
const MAGENTA = '\x1b[35m'
const GRAY = '\x1b[90m'
const GREEN = '\x1b[32m'

export default {
  id: 'sess-pi-bench',
  created_at: ago(20),
  command: ['pi'],
  cwd: '/home/user/dev/my-other-project',
  workspace_root: '/home/user/dev/my-other-project',
  remotes: { origin: 'github.com/acme/my-other-project' },
  kind: 'pi',
  alive: true,
  pid: 8821,
  exit_code: null,
  started_at: ago(20),
  exited_at: null,
  title: 'autoresearch benchmark',
  subtitle: '',
  status: { label: '', working: true },
  unread: false,
  socket_path: '/tmp/gmux-sessions/mock.sock',
  peer: 'server',
  mockActive: true,
  terminal: [
    `${GRAY}╭──────────────────────────────────────────────────────╮${RST}`,
    `${GRAY}│${RST} ${BOLD}${MAGENTA}●${RST} ${BOLD}pi${RST} ${DIM}— autoresearch benchmark${RST}${GRAY}                          │${RST}`,
    `${GRAY}╰──────────────────────────────────────────────────────╯${RST}`,
    ``,
    `Running benchmark suite across 4 configurations…`,
    ``,
    `  ${GREEN}✓${RST} baseline                   ${DIM}142ms avg${RST}`,
    `  ${GREEN}✓${RST} with-cache                 ${DIM} 38ms avg${RST}`,
    `  ${BOLD}⠋${RST} parallel-4                 ${DIM}running…${RST}`,
    `    parallel-8                 ${DIM}pending${RST}`,
  ].join('\n'),
} satisfies MockSession
