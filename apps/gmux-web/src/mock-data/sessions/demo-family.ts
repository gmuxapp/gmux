// Agent families for mock mode: a 5-deep chain (exercises the header's
// collapsed `…` crumb, every dot state, and a long title) plus a shallow
// long-titled pair (crumb truncation). Without these, `?mock` has no
// family at all and the header breadcrumbs / family panel can't be seen.
import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const DIM = '\x1b[2m'

function fam(over: Partial<MockSession> & Pick<MockSession, 'id' | 'title'>): MockSession {
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
    semantic_agent: true,
    terminal: `${DIM}(family demo)${RST}`,
    ...over,
  }
}

export const DEMO_FAMILY: MockSession[] = [
  fam({ id: 'fam0root', title: 'orchestrator', last_output_at: ago(40), status: { active: true } }),
  fam({ id: 'fam1kid', title: 'implement drawer', parent_session_id: 'fam0root', unread: true, last_output_at: ago(2) }),
  fam({ id: 'fam2kid', title: 'wire up the protocol adapter layer end to end', parent_session_id: 'fam1kid', status: { active: true }, last_output_at: ago(1) }),
  fam({ id: 'fam3kid', title: 'refactor session store', parent_session_id: 'fam2kid', last_output_at: ago(20) }),
  fam({
    id: 'fam4kid',
    title: 'investigate a really long descendant title that should truncate somewhere sensible',
    parent_session_id: 'fam3kid', status: { active: false, error: true }, last_output_at: ago(5),
  }),
  // Long-titled one-parent chain (shallow case).
  fam({ id: 'famAroot', title: 'a genuinely very long root agent title for truncation checks', last_output_at: ago(60) }),
  fam({ id: 'famAkid', title: 'child of the long-titled root with its own long title', parent_session_id: 'famAroot', unread: true, last_output_at: ago(4) }),
]
