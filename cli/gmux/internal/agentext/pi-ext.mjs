// gmux pi session extension
// ----------------------------------------------------------------------------
// The authoritative source of session state for pi, replacing the agent-shim's
// fs-syscall inference and the daemon's scrollback matching. pi knows exactly
// which conversation it holds and what it's doing; this extension forwards
// that to the gmux runner so attribution, title, and status are all push-based
// and exact.
//
// How it gets loaded (set by the gmux runner when it spawns pi):
//   pi -e /abs/path/pi-ext.mjs          (extensions accumulate; coexists with
//                                         the user's own -e extensions)
//
// Socket: GMUX_SESSION_SOCK, set by the runner. We use a var distinct from the
// shim's GMUX_RUNNER_SOCK because the shim deletes GMUX_RUNNER_SOCK from the
// env at bootstrap (to disarm children) before pi loads this extension.
//
// Events posted to POST /shim/event on the runner socket:
//   { op: "session", path, id, name, cwd, reason }  on bind (start/switch/fork)
//   { op: "status", working, unread, error, title } on agent loop start/end
//
// It is fire-and-forget: a failed POST never throws back into pi.
// ----------------------------------------------------------------------------

import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const http = require("http");

export default function (pi) {
  const sock = process.env.GMUX_SESSION_SOCK;
  if (!sock) return; // not launched by gmux → no-op

  // --- session identity: which conversation pi is bound to ----------------
  // getSessionFile() is the resolved absolute path of the active conversation,
  // or undefined for a brand-new session whose file isn't written yet (the
  // first agent_end below picks it up once it exists).
  function reportSession(reason, ctx) {
    let file, id, name, cwd;
    try {
      const sm = ctx.sessionManager;
      file = sm.getSessionFile();
      id = sm.getSessionId();
      name = sm.getSessionName();
      cwd = sm.getCwd();
    } catch {
      return;
    }
    if (!file) return; // nothing to attribute yet
    post(sock, { op: "session", path: String(file), id, name, cwd, reason });
  }

  // session_start: initial bind (a resume launch already has a file here).
  pi.on("session_start", (_ev, ctx) => reportSession("start", ctx));
  // session_switch: the user picked another conversation (/resume select or
  // /new). reason is "new" | "resume". The case the shim's read inference
  // misses, since pi serves the pick from cache without re-reading the file.
  pi.on("session_switch", (ev, ctx) => reportSession(ev.reason ?? "switch", ctx));
  // session_fork: branched into a new file off the current one.
  pi.on("session_fork", (_ev, ctx) => reportSession("fork", ctx));

  // --- status: drive the sidebar busy/idle/unread/error directly ----------
  // Replaces parsing the JSONL file for status. pi's agent loop bounds map
  // onto the sidebar states; agent_end carries the final messages so we read
  // the terminal stopReason without touching disk.
  pi.on("agent_start", () => post(sock, { op: "status", working: true }));

  pi.on("agent_end", (ev, ctx) => {
    let stopReason;
    const msgs = ev.messages ?? [];
    for (let i = msgs.length - 1; i >= 0; i--) {
      if (msgs[i]?.role === "assistant") {
        stopReason = msgs[i].stopReason;
        break;
      }
    }
    let name;
    try {
      name = ctx.sessionManager.getSessionName();
    } catch {}
    // stop  → finished, mark unread.   aborted → idle, no unread (user Esc).
    // error → pi exhausted its retries and gave up → red dot.
    post(sock, {
      op: "status",
      working: false,
      unread: stopReason === "stop",
      error: stopReason === "error",
      title: name || undefined,
    });
    // A brand-new session's file exists by now; make sure it's attributed.
    reportSession("activity", ctx);
  });
}

function post(socketPath, event) {
  try {
    const body = Buffer.from(JSON.stringify(event), "utf8");
    const req = http.request({
      socketPath,
      path: "/shim/event",
      method: "POST",
      headers: { "content-type": "application/json", "content-length": body.length },
    });
    req.on("error", () => {}); // never surface transport errors into pi
    req.end(body);
  } catch {
    // swallow — the extension must never break pi
  }
}
