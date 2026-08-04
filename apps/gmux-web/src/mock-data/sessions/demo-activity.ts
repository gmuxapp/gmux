// Extra mock sessions exercising the activity spectrum: unread,
// working, and idle at various recency-bucket ages. All on 'laptop'
// so `?mock&host=laptop` localizes them into the local projects.
import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const DIM = '\x1b[2m'

function demo(over: Partial<MockSession> & Pick<MockSession, 'id' | 'title'>): MockSession {
  return {
    created_at: ago(120),
    command: ['claude'],
    cwd: '/home/user/dev/my-project',
    workspace_root: '/home/user/dev/my-project',
    remotes: { origin: 'github.com/acme/my-project' },
    adapter: 'claude',
    project_slug: 'my-project',
    alive: true,
    pid: 4242,
    exit_code: null,
    started_at: ago(120),
    exited_at: null,
    subtitle: '',
    status: { active: false },
    unread: false,
    socket_path: '/tmp/gmux-sessions/mock.sock',
    peer: 'laptop',
    terminal: `${DIM}(demo session)${RST}`,
    ...over,
  }
}

export const DEMO_ACTIVITY: MockSession[] = [
  demo({
    id: '1w08oj4u', title: 'fix flaky e2e test',
    unread: true, last_output_at: ago(3),
  }),
  demo({
    id: '1b9xfugh', title: 'review PR feedback',
    cwd: '/home/user/dev/openclaw', workspace_root: '/home/user/dev/openclaw',
    remotes: { origin: 'github.com/acme/openclaw' },
    project_slug: 'openclaw',
    unread: true, last_output_at: ago(14),
  }),
  demo({
    id: '1hsgp7y3', title: 'migrate settings schema',
    status: { active: true }, last_output_at: ago(1), mockActive: true,
  }),
  demo({
    id: '10r7grsg', title: 'spike: webgl renderer',
    last_output_at: ago(25),
  }),
  demo({
    id: '1funfqki', title: 'update changelog',
    cwd: '/home/user/dev/openclaw', workspace_root: '/home/user/dev/openclaw',
    remotes: { origin: 'github.com/acme/openclaw' },
    project_slug: 'openclaw',
    created_at: ago(60 * 6), started_at: ago(60 * 6),
    last_output_at: ago(60 * 5),
  }),
  demo({
    id: '17i0p4fp', title: 'debug ws reconnect',
    created_at: ago(60 * 27), started_at: ago(60 * 27),
    last_output_at: ago(60 * 26),
  }),
  demo({
    id: '1f4mvwv1', title: 'shell: profiling',
    adapter: 'shell', command: ['bash'],
    created_at: ago(60 * 24 * 5), started_at: ago(60 * 24 * 5),
    last_output_at: ago(60 * 24 * 4),
  }),
  demo({
    id: '1eb5auvb', title: 'refactor launcher',
    alive: false, resumable: true, pid: null, exit_code: 0,
    created_at: ago(60 * 31), started_at: ago(60 * 31),
    exited_at: ago(60 * 30), last_output_at: ago(60 * 30),
  }),
]
