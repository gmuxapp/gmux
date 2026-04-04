import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const DIM = '\x1b[2m'
const GREEN = '\x1b[32m'

export default {
  id: 'sess-openclaw-cfg',
  created_at: ago(10),
  command: ['openclaw', 'configure'],
  cwd: '/home/user/dev/openclaw',
  workspace_root: '/home/user/dev/openclaw',
  remotes: { origin: 'github.com/acme/openclaw' },
  kind: 'shell',
  alive: true,
  pid: 9100,
  exit_code: null,
  started_at: ago(10),
  exited_at: null,
  title: 'openclaw configure',
  subtitle: '',
  status: { label: '', working: false },
  unread: false,
  socket_path: '/tmp/gmux-sessions/mock.sock',
  peer: 'server',
  terminal: [
    `${GREEN}✓${RST} Configuration written to openclaw.toml`,
    `${GREEN}✓${RST} API key validated`,
    `${GREEN}✓${RST} Model endpoints configured`,
    ``,
    `${DIM}Ready. Run 'openclaw start' to begin.${RST}`,
  ].join('\n'),
} satisfies MockSession
