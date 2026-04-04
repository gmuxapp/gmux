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
  kind: 'claude',
  alive: true,
  pid: 44200,
  exit_code: null,
  started_at: ago(10),
  exited_at: null,
  title: 'design landing page',
  subtitle: '',
  status: { label: '', working: false },
  unread: true,
  socket_path: '/tmp/gmux-sessions/sess-claude-01.sock',
  peer: 'laptop',
  terminal: `${C1}╭─── Claude Code ${C2}v2.1.76 ${C1}──────────────────────────────────────╮${RST}
${C1}│${RST}      ${BOLD}Welcome back!${RST}                                       ${C1}│${RST}
${C1}│${RST}      ${C2}~/dev/my-project${RST}                                     ${C1}│${RST}
${C1}╰──────────────────────────────────────────────────────────────╯${RST}

${C10}● ${RST}I'll redesign the landing page hero section. Let me read the
  current layout first.

  ⎿  ${C2}Read ${BOLD}apps/website/src/pages/index.astro${RST} ${C2}(12.4KB)${RST}

${C11}──────────────────────────────────────────────────────────────${RST}
❯ Looks good, go ahead
${C11}──────────────────────────────────────────────────────────────${RST}`,
} satisfies MockSession
