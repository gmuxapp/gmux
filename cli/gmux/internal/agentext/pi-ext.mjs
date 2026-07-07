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
//   { op: "session", path, id, name, slug, cwd, reason } on bind (session_start)
//                                                         and rename (session_info_changed)
//   { op: "turn", phase: "start" }                       on agent loop start
//   { op: "turn", phase: "end", outcome, title }         on agent loop end
//
// Events posted to POST /acp/ingest (ADR 0021 streaming conversation schema):
//   { op: "message_start", messageId }        on assistant message_start
//   { op: "chunk", messageId, delta }         per assistant text token
//   { op: "thinking_chunk", messageId, delta } per assistant reasoning token
//   { op: "message_end" }                     on assistant message_end
//
// The /acp/ingest channel is the token-level assistant-text feed the runner
// turns into ACP session/update notifications. It is one-way and read-only
// (the extension only observes; the write path is keystrokes, per ADR 0021 §6)
// and, like /hook/event, fire-and-forget — but ordered (see postACP). The full
// contract is documented in docs/acp-conversation-stream.md.
//
// `name`/`title` is pi's session name when it has one; until pi titles the
// conversation we fall back to its first user message (truncated), so a working
// session is identifiable by what it's about rather than a bare cwd. This
// mirrors what codex/claude hooks already report; per ADR 0015 the translation
// from pi's events to the gmux protocol lives here, at the typed-access point.
//
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

  // First user message of the bound conversation, captured once and used as the
  // title until pi names the session. Reset on every bind (a switch/resume/fork
  // is a different conversation whose previous fallback no longer applies).
  let firstUserTitle = "";

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
    const title = name || firstUserTitle;
    // Report the title as the slug source too. pi's session id is a UUID that
    // slugifies into an unreadable URL; without an explicit slug the runner
    // falls back to Slugify(id) and session URLs become UUIDs. The runner
    // slugifies whatever we send, so the raw title is fine here. Mirrors the
    // codex and claude hooks, which send a title-derived slug for the same
    // reason.
    post(sock, {
      op: "session",
      path: String(file),
      id,
      name: title || undefined,
      slug: title || undefined,
      cwd,
      reason,
    });
  }

  // session_start is the one authoritative bind event: pi fires it on startup
  // AND on every switch/new/resume/fork (each preceded by session_shutdown of
  // the old session), carrying the new file and a reason of
  // startup | new | resume | fork. This is what catches a cache-served
  // /resume-select, where no file is read for an fs probe to observe.
  pi.on("session_start", (ev, ctx) => {
    firstUserTitle = ""; // new bind → forget the previous conversation's fallback
    reportSession(ev?.reason ?? "start", ctx);
  });

  // /name (or any extension calling setSessionName) renames the bound session
  // without running a turn; session_start/agent_end won't fire until the next
  // interaction, so forward the rename immediately or the sidebar title stays
  // stale.
  pi.on("session_info_changed", (_ev, ctx) => reportSession("rename", ctx));

  // --- turn lifecycle: drive the sidebar busy/idle without parsing the file -
  // pi's agent loop bounds map onto the sidebar's working/idle; agent_end
  // carries the final messages so we read the terminal stopReason off-disk and
  // normalize it. The runner decides what each outcome means for the sidebar.
  pi.on("agent_start", () => post(sock, { op: "turn", phase: "start" }));

  // --- streaming assistant text (ADR 0021) --------------------------------
  // Forward token-level assistant text to the runner's ACP ingest channel.
  // message_start/message_end bound the assistant message; message_update
  // carries the token-by-token stream via event.assistantMessageEvent, whose
  // text_delta variant holds incremental visible text and whose thinking_delta
  // variant holds incremental reasoning — both in `.delta` (verified against
  // pi-ai's AssistantMessageEvent union, pi 0.80.3). We forward text as `chunk`
  // and thinking as `thinking_chunk`; toolcall deltas are a later slice.
  //
  // messageId correlates the deltas of one assistant message so the runner and
  // frontend can group them (thinking and text share the message's id, ordered
  // by arrival). pi's in-memory AssistantMessage has no id field, so we mint a
  // monotonic per-turn counter on each assistant message_start. message_update
  // fires only for assistant messages (per pi's event protocol).
  let acpMsgId = "";
  let acpMsgSeq = 0;

  pi.on("message_start", (ev) => {
    if (ev?.message?.role !== "assistant") return;
    acpMsgId = `m${++acpMsgSeq}`;
    postACP(sock, { op: "message_start", messageId: acpMsgId });
  });

  pi.on("message_update", (ev) => {
    const ame = ev?.assistantMessageEvent;
    if (!ame || !ame.delta) return;
    const op =
      ame.type === "text_delta" ? "chunk" : ame.type === "thinking_delta" ? "thinking_chunk" : null;
    if (!op) return; // ignore start/end/toolcall/other stream events
    // Open a message implicitly if a delta somehow precedes message_start.
    if (!acpMsgId) acpMsgId = `m${++acpMsgSeq}`;
    postACP(sock, { op, messageId: acpMsgId, delta: ame.delta });
  });

  pi.on("message_end", (ev) => {
    if (ev?.message?.role !== "assistant") return;
    postACP(sock, { op: "message_end", messageId: acpMsgId });
    acpMsgId = "";
  });

  pi.on("agent_end", (ev, ctx) => {
    const msgs = ev.messages ?? [];
    let stopReason;
    for (let i = msgs.length - 1; i >= 0; i--) {
      if (msgs[i]?.role === "assistant") {
        stopReason = msgs[i].stopReason;
        break;
      }
    }
    // Capture the first user message once, as the title fallback until pi names
    // the session. ev.messages on the first turn carries the opening prompt.
    if (!firstUserTitle) {
      for (const m of msgs) {
        const t = extractUserText(m);
        if (t) {
          firstUserTitle = truncateTitle(t);
          break;
        }
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
    const title = name || firstUserTitle;
    post(sock, { op: "turn", phase: "end", outcome, title: title || undefined });
    // A brand-new session's file exists by now; make sure it's attributed.
    reportSession("activity", ctx);
  });
}

// extractUserText pulls the text of a pi user message. content is either a
// plain string or an array of typed blocks; mirrors pi.go's extractFirstUserText.
function extractUserText(msg) {
  if (!msg || msg.role !== "user") return "";
  const c = msg.content;
  if (typeof c === "string") return c;
  if (Array.isArray(c)) {
    for (const b of c) {
      if (b && b.type === "text" && b.text) return b.text;
    }
  }
  return "";
}

// truncateTitle collapses whitespace and caps length at a word boundary, with
// an ellipsis. Mirrors pi.go's truncateTitle (maxLen 80) so the live title and
// the one ParseConversationFile recovers after a restart agree. Go measures length
// in UTF-8 bytes, so we operate on bytes too (JS string length is UTF-16 code
// units, which would diverge for non-ASCII prompts near the boundary).
function truncateTitle(s) {
  s = s.replace(/\s+/g, " ").trim();
  const maxLen = 80;
  const bytes = Buffer.from(s, "utf8");
  if (bytes.length <= maxLen) return s;
  // Go: strings.LastIndex(s[:maxLen], " ") — last space byte within the cap.
  let cut = bytes.lastIndexOf(0x20, maxLen - 1);
  if (cut < maxLen / 2) cut = maxLen;
  return bytes.subarray(0, cut).toString("utf8") + "…";
}

function post(socketPath, event) {
  postTo(socketPath, "/hook/event", event);
}

// postACP forwards a streaming-conversation event to the runner's ACP ingest
// channel. Unlike /hook/event (low-frequency, order-independent facts), token
// deltas MUST arrive in order: independent fire-and-forget requests can
// complete out of order and corrupt the reassembled text. So ACP posts are
// serialized through a promise chain — each request is issued only after the
// previous one has completed. Still fire-and-forget: errors are swallowed and
// never surface into pi. Ordering is per-message and cheap over a local
// socket.
let acpChain = Promise.resolve();
function postACP(socketPath, event) {
  acpChain = acpChain.then(() => postToAwait(socketPath, "/acp/ingest", event)).catch(() => {});
}

// postToAwait issues a POST and resolves when the request completes (response
// received or error), so callers can chain to preserve ordering.
function postToAwait(socketPath, path, event) {
  return new Promise((resolve) => {
    try {
      const body = Buffer.from(JSON.stringify(event), "utf8");
      const req = http.request(
        {
          socketPath,
          path,
          method: "POST",
          headers: { "content-type": "application/json", "content-length": body.length },
        },
        (res) => {
          res.on("end", resolve);
          res.on("error", resolve);
          res.resume(); // drain
        },
      );
      req.on("error", resolve); // never surface transport errors into pi
      req.end(body);
    } catch {
      resolve();
    }
  });
}

function postTo(socketPath, path, event) {
  try {
    const body = Buffer.from(JSON.stringify(event), "utf8");
    const req = http.request({
      socketPath,
      path,
      method: "POST",
      headers: { "content-type": "application/json", "content-length": body.length },
    });
    req.on("error", () => {}); // never surface transport errors into pi
    req.end(body);
  } catch {
    // swallow — the extension must never break pi
  }
}
