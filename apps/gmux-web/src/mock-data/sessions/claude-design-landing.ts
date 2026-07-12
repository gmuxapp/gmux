import { type MockSession, ago } from '../types'

const C1 = '\x1b[0;38;2;215;119;87m'
const C2 = '\x1b[0;38;2;153;153;153m'
const RST = '\x1b[0m'
const BOLD = '\x1b[0;1m'
const C10 = '\x1b[0;38;2;255;255;255m'
const C11 = '\x1b[0;38;2;136;136;136m'

export default {
  id: 'sess-claude-01',
  created_at: ago(10),
  command: ['claude'],
  cwd: '/home/user/dev/my-project',
  workspace_root: '/home/user/dev/my-project',
  remotes: { origin: 'github.com/acme/my-project' },
  adapter: 'claude',
  alive: true,
  pid: 44200,
  exit_code: null,
  started_at: ago(10),
  exited_at: null,
  title: 'design landing page',
  subtitle: '',
  status: { working: false },
  unread: true,
  project_slug: 'my-project',
  last_activity_at: ago(3),
  socket_path: '/tmp/gmux-sessions/sess-claude-01.sock',
  // Sized to 38 columns: the landing page's mobile hero captures this
  // session at a viewport that fits exactly 38 xterm columns, so lines
  // must stay ≤38 chars to fill the terminal without wrapping.
  terminal: `${C1}╭─ Claude Code ${C2}v2.1.76 ${C1}──────────────╮${RST}
${C1}│${RST}   ${BOLD}Welcome back!${RST}                    ${C1}│${RST}
${C1}│${RST}   ${C2}~/dev/my-project${RST}                 ${C1}│${RST}
${C1}╰────────────────────────────────────╯${RST}

${C2}❯ update the landing page for 2.0${RST}

${C10}● ${RST}I'll rework the hero section and
  tighten the copy.

  ⎿  ${C2}Read ${BOLD}pages/index.astro${RST}
  ⎿  ${C2}Edit ${BOLD}pages/index.astro${RST} ${C2}(+41 -18)${RST}
  ⎿  ${C2}Bash ${BOLD}pnpm build${RST} ${C2}✓${RST}

${C10}● ${RST}The new hero is in. Want me to
  regenerate the screenshots too?

${C11}────────────────────────────────────${RST}
❯ 
${C11}────────────────────────────────────${RST}`,
  cursorX: 2,
  cursorY: 19,
} satisfies MockSession
