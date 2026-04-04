import { type MockSession, ago } from '../types'

const DIM = '\x1b[0;2m'
const RST = '\x1b[0m'
const BOLD = '\x1b[0;1m'
const CYAN = '\x1b[0;36m'
const C1 = '\x1b[0;48;2;43;48;53m'
const C2 = '\x1b[0;1;2;48;2;43;48;53m'
const HI_BLUE = '\x1b[0;94m'
const ITALIC = '\x1b[0;3m'
const C3 = '\x1b[0;1;38;2;95;100;106m'
const C4 = '\x1b[0;1;38;2;211;216;222m'
const C5 = '\x1b[0;1;38;2;194;199;205m'
const C6 = '\x1b[0;1;38;2;150;155;161m'
const C7 = '\x1b[0;1;38;2;51;56;62m'
const C8 = '\x1b[0;1;38;2;34;39;45m'
const C9 = '\x1b[0;1;48;2;43;48;53m'
const C10 = '\x1b[0;2;48;2;43;48;53m'

export default {
  id: 'sess-4ab4995b',
  created_at: ago(0),
  command: ["codex"],
  cwd: '/home/user/dev/my-project',
  workspace_root: '/home/user/dev/my-project',
  remotes: { origin: 'github.com/acme/my-project' },
  kind: 'codex',
  alive: true,
  pid: 568143,
  exit_code: null,
  started_at: ago(0),
  exited_at: null,
  title: "refactor adapters",
  subtitle: "",
  status: { label: "", working: true },
  unread: false,
  socket_path: '/tmp/gmux-sessions/sess-4ab4995b.sock',
  peer: 'laptop',
  cursorX: 2,
  cursorY: 70,
  terminal: `
${DIM}╭───────────────────────────────────────╮${RST}
${DIM}│ directory: ${RST}~/dev/gmux                 ${DIM}│${RST}
${DIM}╰───────────────────────────────────────╯${RST}

${C1}                                                                                                                                      ${RST}
${C2}› ${C1}I'd like to refactor the adapter system. What do you see that's worth refactoring?                                                  ${RST}
${C1}                                                                                                                                      ${RST}

${DIM}• ${RST}I’m going to inspect the adapter-related codepaths and map
  how state, interfaces, and lifecycle are currently handled
  before suggesting refactors.

${DIM}• ${BOLD}Explored${RST}
  ${DIM}└ ${CYAN}List ${RST}.
    ${CYAN}Search ${RST}adapter|Adapter ${DIM}in ${RST}.

${DIM}──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────${RST}

${DIM}• ${RST}I’ve mapped the main adapter surface area. Next I’m reading the interface definitions, registry, and one runtime path on each side
  (${CYAN}gmux ${RST}and ${CYAN}gmuxd${RST}) to separate structural issues from adapter-specific duplication.

${DIM}• ${BOLD}Explored${RST}
  ${DIM}└ ${CYAN}Read ${RST}adapter.go${DIM}, ${RST}capabilities.go${DIM}, ${RST}shell.go${DIM}, ${RST}registry.go${DIM}, ${RST}claude.go${DIM}, ${RST}codex.go${DIM}, ${RST}pi.go${DIM}, ${RST}filemon.go${DIM}, ${RST}scanner.go${DIM}, ${RST}store.go

${DIM}──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────${RST}

${DIM}• ${RST}The core shape is clear now: the adapter package has a small base interface, but most of the real behavior is spread across optional
  capability interfaces plus daemon-side type assertions. I’m drilling into that boundary next because that’s where most of the
  structural refactor value sits.

${DIM}• ${BOLD}Explored${RST}
  ${DIM}└ ${CYAN}Read ${RST}main.go${DIM}, ${RST}ptyserver.go${DIM}, ${RST}scanner.go${DIM}, ${RST}capabilities.go${DIM}, ${RST}store.go${DIM}, ${RST}filemon.go${DIM}, ${RST}claude.go${DIM}, ${RST}codex.go${DIM}, ${RST}pi.go${DIM}, ${RST}shell.go

${DIM}───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────${RST}

${C3}• ${C4}Wo${C5}r${C6}k${C3}i${C7}n${C8}g ${DIM}(6s • esc to interrupt)${RST}

${C1}                                                                                                                           ${RST}
${C9}›${C1} ${C10}Use /skills to list available skills${C1}                                                                                     ${RST}
${C1}                                                                                                                           ${RST}
  ${DIM}gpt-5.4 default · 67% left · ~/dev/gmux${RST}`,
} satisfies MockSession
