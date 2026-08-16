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
  // Processes owned by agents in the chain: the sidebar's family
  // activity row counts a running one under `$` (subagents get a dot),
  // and the family drawer shows them with the same `$` glyph.
  fam({
    id: 'fam0proc', title: 'pnpm test --watch', parent_session_id: 'fam0root',
    semantic_agent: false, adapter: 'shell', command: ['pnpm', 'test'],
    status: { active: true }, last_output_at: ago(3),
  }),
  fam({
    id: 'fam1proc', title: 'tail -f daemon.log', parent_session_id: 'fam1kid',
    semantic_agent: false, adapter: 'shell', command: ['tail', '-f', 'daemon.log'],
    last_output_at: ago(30),
  }),
  // Long-titled one-parent chain (shallow case).
  fam({ id: 'famAroot', title: 'a genuinely very long root agent title for truncation checks', last_output_at: ago(60) }),
  fam({ id: 'famAkid', title: 'child of the long-titled root with its own long title', parent_session_id: 'famAroot', unread: true, last_output_at: ago(4) }),
  // A promoted family member: renders as its own root row while the
  // organizational parent edge stays on the session. Exercises the ⋮ menu's
  // "Return to family" action and the sidebar's promoted projection.
  fam({
    id: 'famApromoted', title: 'promoted research spike', parent_session_id: 'famAroot',
    promoted_to_root: true, last_output_at: ago(15),
  }),
  // Process-only family: the summary line drops the empty segment and
  // reads just "2 processes".
  fam({ id: 'famBroot', title: 'build watcher agent', last_output_at: ago(90) }),
  fam({
    id: 'famBproc1', title: 'vite build --watch', parent_session_id: 'famBroot',
    semantic_agent: false, adapter: 'shell', command: ['vite', 'build'],
    status: { active: true }, last_output_at: ago(11),
  }),
  fam({
    id: 'famBproc2', title: 'gofmt -l ./...', parent_session_id: 'famBroot',
    semantic_agent: false, adapter: 'shell', command: ['gofmt'], unread: true, last_output_at: ago(9),
  }),
  // A child working outside every project (no stamp, no matching rule):
  // promoting it would give it no sidebar row and no routable URL, so the
  // ⋮ menu offers Promote to root blocked, with the reason.
  fam({
    id: 'famBoutside', title: 'scratch probe in /tmp', parent_session_id: 'famBroot',
    cwd: '/tmp/scratch', workspace_root: '/tmp/scratch', remotes: undefined,
    project_slug: undefined, last_output_at: ago(7),
  }),
]
