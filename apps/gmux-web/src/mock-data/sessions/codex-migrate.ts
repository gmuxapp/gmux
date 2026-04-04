import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const BOLD = '\x1b[1m'
const DIM = '\x1b[2m'
const RED = '\x1b[31m'
const CYAN = '\x1b[36m'
const GRAY = '\x1b[90m'

export default {
  id: 'sess-codex-pnpm',
  created_at: ago(25),
  command: ['codex'],
  cwd: '/home/user/dev/my-project',
  workspace_root: '/home/user/dev/my-project',
  remotes: { origin: 'github.com/acme/my-project' },
  kind: 'codex',
  alive: true,
  pid: 8830,
  exit_code: null,
  started_at: ago(25),
  exited_at: null,
  title: 'migrate to convex',
  subtitle: '',
  status: { label: '', working: false },
  unread: false,
  socket_path: '/tmp/gmux-sessions/mock.sock',
  peer: 'server',
  terminal: [
    `${GRAY}╭──────────────────────────────────────────────────────╮${RST}`,
    `${GRAY}│${RST} ${BOLD}codex${RST} ${DIM}— migrate to pnpm${RST}${GRAY}                                │${RST}`,
    `${GRAY}╰──────────────────────────────────────────────────────╯${RST}`,
    ``,
    `Migrating from npm to pnpm…`,
    ``,
    `${RED}ERROR${RST} Lockfile conflict in packages/shared:`,
    `  ${DIM}npm resolved express@4.18.2${RST}`,
    `  ${DIM}pnpm resolved express@4.19.1${RST}`,
    ``,
    `${BOLD}${CYAN}⠋ Investigating the conflict…${RST}`,
  ].join('\n'),
} satisfies MockSession
