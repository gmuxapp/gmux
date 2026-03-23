import { type MockSession, ago } from '../types'

const RST = '\x1b[0m'
const BOLD = '\x1b[1m'
const DIM = '\x1b[2m'
const WHITE = '\x1b[37m'
const BG_GREEN = '\x1b[42m'
const BG_RED = '\x1b[41m'

export default {
  id: 'sess-s1t2u3',
  created_at: ago(25),
  command: ['pytest', 'tests/', '-v'],
  cwd: '/home/user/dev/api',
  workspace_root: '/home/user/dev/api',
  kind: 'shell',
  alive: true,
  pid: 45678,
  exit_code: null,
  started_at: ago(25),
  exited_at: null,
  title: 'pytest tests/ -v',
  subtitle: '42/50 passing',
  status: { label: '42/50 passing', working: true },
  unread: false,
  socket_path: '/tmp/gmux-sessions/mock.sock',
  terminal: [
    `${BOLD}${WHITE}$ pytest tests/ -v${RST}`,
    `${DIM}================================ test session starts =================================${RST}`,
    `${DIM}platform linux -- Python 3.12.3, pytest-8.1.1${RST}`,
    `${DIM}collected 50 items${RST}`,
    ``,
    `tests/test_api.py::test_health_check ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_api.py::test_create_user ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_api.py::test_get_user ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_api.py::test_update_user ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_api.py::test_delete_user ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_api.py::test_list_users ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_auth.py::test_login ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_auth.py::test_logout ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_auth.py::test_refresh_token ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_auth.py::test_invalid_token ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_auth.py::test_expired_token ${BG_RED}${WHITE} FAILED ${RST}`,
    `tests/test_models.py::test_user_model ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_models.py::test_post_model ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_models.py::test_comment_model ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_models.py::test_relationships ${BG_GREEN}${WHITE} PASSED ${RST}`,
    `tests/test_rate_limit.py::test_rate_limit_basic ${BOLD}${WHITE}RUNNING${RST}`,
  ].join('\n'),
} satisfies MockSession
