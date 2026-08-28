// Agent families for mock mode: a 5-deep chain (exercises the header's
// collapsed `…` crumb, every attention state, and a long title) plus a shallow
// long-titled pair (crumb truncation). Without these, `?mock` has no
// family at all and the header breadcrumbs / family panel can't be seen.
import { ago, type MockSession } from '../types'

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
  // Active-error is a transient retry: the selected row stays in the active
  // family bucket but renders the same hollow active ring in red.
  fam({ id: 'fam2kid', title: 'wire up the protocol adapter layer end to end', parent_session_id: 'fam1kid', status: { active: true, error: true }, last_output_at: ago(1) }),
  fam({ id: 'fam3kid', title: 'refactor session store', parent_session_id: 'fam2kid', last_output_at: ago(20) }),
  fam({
    id: 'fam4kid',
    title: 'investigate a really long descendant title that should truncate somewhere sensible',
    // An unread terminal error supplies filterable waiting-error attention;
    // error without unread is an acknowledged outcome and has no family bucket.
    parent_session_id: 'fam3kid', status: { active: false, error: true }, unread: true, last_output_at: ago(5),
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
    id: 'famApromoted', title: 'promoted research spike', parent_session_id: undefined,
    launched_from_session_id: 'famAroot', last_output_at: ago(15),
  }),
  // Process-only family: summaries report the one running command; the
  // panel's process filter also reveals the finished command.
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
  // Finished-only process family: no running summary outside the panel;
  // inside it, the gray `$ processes` control opens Finished directly.
  fam({ id: 'famQroot', title: 'quiet task runner', last_output_at: ago(150) }),
  fam({
    id: 'famQproc1', title: 'pnpm lint', parent_session_id: 'famQroot',
    semantic_agent: false, adapter: 'shell', command: ['pnpm', 'lint'],
    unread: true, last_output_at: ago(140),
  }),
  fam({
    id: 'famQproc2', title: 'go test ./...', parent_session_id: 'famQroot',
    semantic_agent: false, adapter: 'shell', command: ['go', 'test', './...'],
    last_output_at: ago(145),
  }),
  // Enough history to prove the Finished section keeps its own fold.
  ...Array.from({ length: 26 }, (_, i) => fam({
    id: `famQhist${String(i).padStart(2, '0')}`,
    title: `historical task ${i + 1}`,
    parent_session_id: 'famQroot', semantic_agent: false, adapter: 'shell',
    command: ['task', String(i + 1)], last_output_at: ago(150 + i),
  })),
  // A saturated running section plus one selected finished process proves
  // that selection displaces a running row rather than vanishing at budget 0.
  fam({ id: 'famSroot', title: 'saturated task runner', last_output_at: ago(210) }),
  ...Array.from({ length: 25 }, (_, i) => fam({
    id: `famSrun${String(i).padStart(2, '0')}`,
    title: `running task ${i + 1}`,
    parent_session_id: 'famSroot', semantic_agent: false, adapter: 'shell',
    command: ['watch', String(i + 1)], status: { active: true }, last_output_at: ago(180 + i),
  })),
  fam({
    id: 'famSfinished', title: 'selected finished task', parent_session_id: 'famSroot',
    semantic_agent: false, adapter: 'shell', command: ['task', 'done'], last_output_at: ago(209),
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
