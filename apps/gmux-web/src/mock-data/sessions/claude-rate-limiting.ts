import { type MockSession, ago } from '../types'

const C1 = '\x1b[0;38;2;215;119;87m'
const C2 = '\x1b[0;38;2;153;153;153m'
const RST = '\x1b[0m'
const C3 = '\x1b[0;2;38;2;215;119;87m'
const C4 = '\x1b[0;1;2;38;2;215;119;87m'
const BOLD = '\x1b[0;1m'
const C5 = '\x1b[0;38;2;215;119;87;48;2;0;0;0m'
const C6 = '\x1b[0;38;2;80;80;80;48;2;55;55;55m'
const C7 = '\x1b[0;38;2;255;255;255;48;2;55;55;55m'
const C8 = '\x1b[0;48;2;55;55;55m'
const C9 = '\x1b[0;1;38;2;153;153;153m'
const C10 = '\x1b[0;38;2;255;255;255m'
const C11 = '\x1b[0;38;2;136;136;136m'

export default {
  id: 'sess-85a6ba5a',
  created_at: ago(8),
  command: ['claude'],
  cwd: '/home/user/dev/gmux',
  workspace_root: '/home/user/dev/gmux',
  kind: 'shell',
  alive: true,
  pid: 133425,
  exit_code: null,
  started_at: ago(8),
  exited_at: null,
  title: '✳ What is in this image? @image.png',
  subtitle: '',
  status: { label: '', working: false },
  unread: false,
  socket_path: '/tmp/gmux-sessions/sess-85a6ba5a.sock',
  cursorX: 10,
  cursorY: 22,
  terminal: `${C1}╭─── Claude Code ${C2}v2.1.76 ${C1}────────────────────────────────────────────────────────────╮${RST}
${C1}│                                                    ${C3}│ ${C4}Tips for getting started      ${C1}│${RST}
${C1}│                ${BOLD}Welcome back mgabor!                ${C4}│ ${RST}Run /init to create a CLAUDE… ${C1}│${RST}
${C1}│                                                    ${C3}│ ───────────────────────────── ${C1}│${RST}
${C1}│                       ▐${C5}▛███▜${C1}▌                      ${C3}│ ${C4}Recent activity               ${C1}│${RST}
${C1}│                      ▝▜${C5}█████${C1}▛▘                     ${C3}│ ${C2}No recent activity            ${C1}│${RST}
${C1}│                        ▘▘ ▝▝                       ${C3}│                               ${C1}│${RST}
${C1}│      ${C2}Opus 4.6 (1M context) with medium effort      ${C3}│                               ${C1}│${RST}
${C1}│                     ${C2}~/dev/gmux                     ${C3}│                               ${C1}│${RST}
${C1}╰────────────────────────────────────────────────────────────────────────────────────╯${RST}

  ${C2}↑ Opus now defaults to 1M context · 5x more room, same pricing${RST}

${C6}❯ ${C7}What is in this image? @image.png${C8}                                                   ${RST}
  ⎿  ${C2}Read ${C9}image.png ${C2}(207.9KB)${RST}

${C10}● ${RST}It's a photo of a white cat with heterochromia (two different colored eyes — one
  appears blue/green and the other golden/amber). The cat is sitting outdoors on grass
   with some plants and a fence visible in the background. It's a nice sunny day shot
  with the light catching the cat's pink-tinged ears.

${C11}──────────────────────────────────────────────────────────────────────────────────────${RST}
❯ Cute! 😺
${C11}──────────────────────────────────────────────────────────────────────────────────────${RST}`,
} satisfies MockSession
