// gmux pi session extension
// ----------------------------------------------------------------------------
// The authoritative source of session state for pi. pi knows exactly which
// conversation it holds and what it's doing; this hook forwards that to the
// gmux runner so attribution, title, and status are all push-based and exact
// — no fs-syscall inference, no scrollback matching.
//
// How it gets loaded (set by the gmux runner when it spawns pi):
//   pi -e /abs/path/pi-ext.mjs          (extensions accumulate; coexists with
//                                         the user's own -e extensions)
//
// Socket: GMUX_SESSION_SOCK, set by the runner.
//
// Events posted to POST /hook/event on the runner socket:
//   { op: "session", path, id, name, cwd, reason }      on bind (session_start)
//   { op: "turn", phase: "start" }                       on agent loop start
//   { op: "turn", phase: "end", outcome, title }         on agent loop end
// outcome is pi's terminal state normalized to a stable vocabulary
// ("completed" | "aborted" | "error"); the runner owns what each means for the
// sidebar (e.g. completed → unread). The extension reports pi facts, not gmux
// policy.
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

  // session_start is the one authoritative bind event: pi fires it on startup
  // AND on every switch/new/resume/fork (each preceded by session_shutdown of
  // the old session), carrying the new file and a reason of
  // startup | new | resume | fork. This is what catches a cache-served
  // /resume-select, where no file is read for an fs probe to observe.
  pi.on("session_start", (ev, ctx) => reportSession(ev?.reason ?? "start", ctx));

  // --- turn lifecycle: drive the sidebar busy/idle without parsing the file -
  // pi's agent loop bounds map onto the sidebar's working/idle; agent_end
  // carries the final messages so we read the terminal stopReason off-disk and
  // normalize it. The runner decides what each outcome means for the sidebar.
  pi.on("agent_start", () => post(sock, { op: "turn", phase: "start" }));

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
    // Normalize pi's stopReason to a stable outcome vocabulary:
    //   stop  → completed (turn finished on its own)
    //   error → error     (pi exhausted retries and gave up)
    //   else  → aborted   (user Esc, or any other non-completion)
    const outcome =
      stopReason === "stop" ? "completed" : stopReason === "error" ? "error" : "aborted";
    post(sock, { op: "turn", phase: "end", outcome, title: name || undefined });
    // A brand-new session's file exists by now; make sure it's attributed.
    reportSession("activity", ctx);
  });
}

function post(socketPath, event) {
  try {
    const body = Buffer.from(JSON.stringify(event), "utf8");
    const req = http.request({
      socketPath,
      path: "/hook/event",
      method: "POST",
      headers: { "content-type": "application/json", "content-length": body.length },
    });
    req.on("error", () => {}); // never surface transport errors into pi
    req.end(body);
  } catch {
    // swallow — the extension must never break pi
  }
}
