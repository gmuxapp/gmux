import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const BOLD = '\x1b[1m'
const DIM = '\x1b[2m'
const GREEN = '\x1b[32m'
const CYAN = '\x1b[36m'
const MAGENTA = '\x1b[35m'
const GRAY = '\x1b[90m'

export default {
  id: 'sess-a1b2c3',
  created_at: ago(45),
  command: ['pi'],
  cwd: '/home/user/dev/gmux/.grove/teak',
  workspace_root: '/home/user/dev/gmux',
  remotes: { origin: 'github.com/gmuxapp/gmux' },
  kind: 'pi',
  alive: true,
  pid: 12345,
  exit_code: null,
  started_at: ago(45),
  exited_at: null,
  title: 'Implement adapter system',
  subtitle: 'iteration 3/10',
  status: { label: 'iteration 3/10', working: true },
  adapter_title: 'claude-sonnet-4',
  unread: false,
  socket_path: '/tmp/gmux-sessions/mock.sock',
  terminal: [
    `${GRAY}╭──────────────────────────────────────────────────────╮${RST}`,
    `${GRAY}│${RST} ${BOLD}${MAGENTA}●${RST} ${BOLD}pi${RST} ${DIM}— implement adapter system${RST}${GRAY}                       │${RST}`,
    `${GRAY}╰──────────────────────────────────────────────────────╯${RST}`,
    ``,
    `${BOLD}${CYAN}⠋ Thinking…${RST}`,
    ``,
    `I'll implement the adapter system with a plugin interface that lets`,
    `different AI backends register themselves. Let me start with the`,
    `core types:`,
    ``,
    `${DIM}${GRAY}src/adapters/types.ts${RST}`,
    `${GREEN}+${RST} export interface Adapter {`,
    `${GREEN}+${RST}   readonly name: string`,
    `${GREEN}+${RST}   readonly version: string`,
    `${GREEN}+${RST}   connect(opts: ConnectOptions): Promise<Session>`,
    `${GREEN}+${RST}   disconnect(): Promise<void>`,
    `${GREEN}+${RST} }`,
    `${GREEN}+${RST}`,
    `${GREEN}+${RST} export interface ConnectOptions {`,
    `${GREEN}+${RST}   model: string`,
    `${GREEN}+${RST}   temperature?: number`,
    `${GREEN}+${RST}   maxTokens?: number`,
    `${GREEN}+${RST} }`,
    ``,
    `Now the registry:`,
    ``,
    `${DIM}${GRAY}src/adapters/registry.ts${RST}`,
    `${GREEN}+${RST} const adapters = new Map<string, Adapter>()`,
    `${GREEN}+${RST}`,
    `${GREEN}+${RST} export function registerAdapter(adapter: Adapter): void {`,
    `${GREEN}+${RST}   if (adapters.has(adapter.name)) {`,
    `${GREEN}+${RST}     throw new Error(\`Adapter "\${adapter.name}" already registered\`)`,
    `${GREEN}+${RST}   }`,
    `${GREEN}+${RST}   adapters.set(adapter.name, adapter)`,
    `${GREEN}+${RST} }`,
  ].join('\n'),
} satisfies MockSession
