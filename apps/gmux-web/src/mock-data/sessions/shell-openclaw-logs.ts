import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const DIM = '\x1b[2m'
const GRAY = '\x1b[90m'

export default {
  id: 'sess-openclaw-logs',
  created_at: ago(20),
  command: ['openclaw', 'logs', '-f'],
  cwd: '/home/user/dev/openclaw',
  workspace_root: '/home/user/dev/openclaw',
  remotes: { origin: 'github.com/acme/openclaw' },
  adapter: 'shell',
  alive: true,
  pid: 9200,
  exit_code: null,
  started_at: ago(20),
  exited_at: null,
  title: 'logs',
  subtitle: '',
  status: { working: false },
  unread: false,
  project_slug: 'openclaw',
  last_activity_at: ago(51),
  socket_path: '/tmp/gmux-sessions/mock.sock',
  peer: 'devcontainer',
  mockActive: true,
  terminal: [
    `${GRAY}[09:14:22]${RST} ${DIM}info${RST}  worker started pid=4821`,
    `${GRAY}[09:14:23]${RST} ${DIM}info${RST}  connected to redis`,
    `${GRAY}[09:14:23]${RST} ${DIM}info${RST}  listening on :8080`,
    `${GRAY}[09:15:01]${RST} ${DIM}info${RST}  POST /v1/infer 200 142ms`,
  ].join('\n'),
} satisfies MockSession
