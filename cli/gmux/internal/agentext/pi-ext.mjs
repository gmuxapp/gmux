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
//   { op: "ready" }                                      on session_start, before
//                                                         anything else
//   { op: "session", path, id, name, slug, cwd, reason } on bind (session_start)
//                                                         and rename (session_info_changed)
//   { op: "turn", phase: "start", turn_seq, trigger, source_bytes,
//     previous_exchanges }                              on agent loop start
//   { op: "turn", phase: "iteration", turn_seq }       on assistant message_end
//   { op: "turn", phase: "steered", turn_seq, text, source_bytes }
//                                                       on a mid-turn user message
//   { op: "turn", phase: "end", turn_seq, outcome,
//     output, truncated, diagnostic, title }             on agent_settled
//
// The turn events are the SOURCE ASSERTION of a turn's identity, boundary and
// result (ADR 0027, 2026-07-28 amendment): gmux never reconstructs a turn's
// answer from the conversation file. `turn_seq` is an extension-local monotonic
// counter binding one activity's start, user boundaries and close together, so
// every downstream consumer pairs facts by it and can never pair a running
// turn's trigger with the previous turn's answer.
//
// Events posted to POST /acp/ingest (ADR 0021 streaming conversation schema):
//   { op: "message_start", messageId }        on assistant message_start
//   { op: "chunk", messageId, delta }         per assistant text token
//   { op: "thinking_chunk", messageId, delta } per assistant reasoning token
//   { op: "tool_call", messageId, toolCallId, toolName, kind, args }  on tool start
//   { op: "tool_call_update", toolCallId, status, output }            on tool end
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
// The boundary is `agent_settled`, not `agent_end`: pi emits `agent_end` per
// retry attempt (a transient provider error emits an error-shaped agent_end and
// pi then retries via agent.continue(), which emits a fresh agent_start), while
// `agent_settled` fires exactly once per run, "after an agent run has fully
// settled and no automatic retry, compaction, or queued continuation will run".
// So: the FIRST agent_start of a run opens the turn, every agent_end refreshes
// the captured message list, and agent_settled closes the turn using the last
// captured list. pi merges queued follow-ups into the running loop, so one
// settled run is exactly one gmux turn.
//
// outcome is pi's terminal state normalized to a stable vocabulary
// ("completed" | "interrupted" | "error"); the runner owns what each means for the
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

  // --- turn identity (source-asserted) -------------------------------------
  // turnSeq is monotonic for the life of this extension instance and never
  // reset, not even on a rebind: it is an identity, and reusing a number after
  // a switch/resume would let a stale close match a fresh turn. The runner's
  // frame is cleared on rebind instead.
  let turnSeq = 0;
  // runOpen is true from the FIRST agent_start of a run until agent_settled.
  // pi emits agent_start again for every retry/continuation of the same run, so
  // without this the retries would each look like a new turn.
  let runOpen = false;
  // heldTrigger is the prompt text captured at before_agent_start, which fires
  // after pi's model/auth preflight and immediately before the loop starts. It
  // is held rather than posted there because a throw between the two would
  // otherwise report a turn that never ran; the active edge stays on
  // agent_start and carries the trigger with it.
  let heldTrigger = "";
  let heldTriggerBytes = 0;
  // settledMessages is the message list of the LAST agent_end of the run — the
  // final attempt's — which is what the settled turn's output and stop reason
  // are read from.
  let settledMessages = [];
  // sawAssistant/triggerNoted separate the loop's own opening user message(s)
  // from additional mid-turn boundaries: pi emits message_start for the prompt
  // right after agent_start, and for every steered/queued message as it enters
  // the loop.
  let sawAssistant = false;
  let triggerNoted = false;

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
    // Report the title as the slug source too (runner slugifies it). Until the
    // session has a title we send no slug: pi's session id is a UUID, and the
    // runner deliberately won't synthesize a slug from it — it leaves the slug
    // empty so the web layer falls back to a sigiled full session ID. Mirrors the
    // codex and claude hooks, which also send a title-derived slug only once
    // they have a title.
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
    rememberNotifier(ctx);
    // Readiness: pi can accept input. Reported FIRST and UNCONDITIONALLY —
    // before reportSession, and regardless of whether pi has a conversation
    // file yet. gmux's semantic actions (`gmux agent prompt/cancel`) block on
    // this event, so tying it to conversation availability would deadlock the
    // first prompt of a brand-new session, whose file is not written until a
    // turn runs. It is safe here because pi installs the editor, key handlers
    // and submit handler and starts its UI *before* it initializes extensions
    // and fires session_start: by the time this runs, keystrokes we deliver
    // land in the composer.
    //
    // Sent on every bind, not just the first. The runner treats repeats as
    // idempotent, and a rebind (switch/new/resume/fork) reaching here is
    // positive evidence the composer is alive.
    post(sock, { op: "ready" });
    firstUserTitle = ""; // new bind → forget the previous conversation's fallback
    // A rebind abandons whatever run was open on the previous conversation: its
    // settled event will never arrive, and reporting it against the new
    // conversation would attribute the old turn's answer to the new one.
    runOpen = false;
    heldTrigger = "";
    heldTriggerBytes = 0;
    settledMessages = [];
    reportSession(ev?.reason ?? "start", ctx);
  });

  // /name (or any extension calling setSessionName) renames the bound session
  // without running a turn; session_start/agent_end won't fire until the next
  // interaction, so forward the rename immediately or the sidebar title stays
  // stale.
  pi.on("session_info_changed", (_ev, ctx) => reportSession("rename", ctx));

  // --- turn lifecycle: drive the sidebar busy/idle without parsing the file -
  // pi's agent loop bounds map onto the sidebar's active/idle; agent_end
  // carries the final messages so we read the terminal stopReason off-disk and
  // normalize it. The runner decides what each outcome means for the sidebar.
  // The prompt text of the run that is about to start. pi validates the model
  // and credentials BEFORE emitting this (agent-session.js: the auth throw
  // precedes emitBeforeAgentStart), and a queued steer/follow-up returns even
  // earlier, so this fires once per real run and never for a prompt that fails
  // preflight.
  pi.on("before_agent_start", (ev) => {
    const trigger = capUserText(ev?.prompt ?? "");
    heldTrigger = trigger.text;
    heldTriggerBytes = trigger.sourceBytes;
  });

  pi.on("agent_start", (_ev, ctx) => {
    if (runOpen) return; // a retry or queued continuation of the SAME run
    turnSeq++;
    runOpen = true;
    sawAssistant = false;
    triggerNoted = false;
    settledMessages = [];
    let previousExchanges;
    try {
      previousExchanges = ctx.sessionManager.getBranch().filter(
        (entry) => entry?.type === "message" && entry?.message?.role === "user"
      ).length;
    } catch {}
    post(sock, {
      op: "turn", phase: "start", turn_seq: turnSeq,
      trigger: heldTrigger,
      source_bytes: heldTriggerBytes,
      previous_exchanges: previousExchanges,
    });
  });

  // pi has no persistent message id, so mint one for each streamed assistant
  // message. These variables are shared by the turn-boundary and ACP handlers
  // because pi permits only one callback per event name.
  let acpMsgId = "";
  let acpMsgSeq = 0;

  // A user message entering a RUNNING loop extends that turn and changes what
  // its answer means — whether gmux delivered it, another agent steered, or a
  // human typed it into the TUI. It is reported as a boundary on the open
  // turn, never as a new turn (pi has one loop, hence one turn).
  pi.on("message_start", (ev) => {
    const msg = ev?.message;
    if (msg?.role === "assistant") {
      if (runOpen) sawAssistant = true;
      acpMsgId = `m${++acpMsgSeq}`;
      postACP(sock, { op: "message_start", messageId: acpMsgId });
      return;
    }
    if (!runOpen) return;
    const text = extractUserText(msg);
    if (!text) return;
    if (!sawAssistant && !triggerNoted) {
      // The loop's own opening prompt, replayed as a message_start right after
      // agent_start. It is the opening boundary, not an additional one.
      triggerNoted = true;
      if (!heldTrigger) {
        const trigger = capUserText(text);
        heldTrigger = trigger.text;
        heldTriggerBytes = trigger.sourceBytes;
      }
      return;
    }
    // User boundaries are report content, not diagnostics: preserve whitespace
    // verbatim up to the source-side live-frame cap.
    const injected = capUserText(text);
    post(sock, {
      op: "turn",
      phase: "steered",
      turn_seq: turnSeq,
      text: injected.text,
      source_bytes: injected.sourceBytes,
    });
  });

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
    if (runOpen) post(sock, { op: "turn", phase: "iteration", turn_seq: turnSeq });
    postACP(sock, { op: "message_end", messageId: acpMsgId });
    acpMsgId = "";
  });

  // --- streaming tool calls (ADR 0021) ------------------------------------
  // pi surfaces tool execution as dedicated events (verified against pi's
  // extension API, @earendil-works/pi-coding-agent): tool_execution_start
  // carries { toolCallId, toolName, args }, tool_execution_end carries
  // { toolCallId, result, isError } where result is an AgentToolResult whose
  // `.content` is an array of text/image blocks. We forward the start as a
  // `tool_call` (in progress) and the end as a `tool_call_update` (terminal
  // status + textual output), keyed by toolCallId so the runner mutates the
  // existing block rather than appending. Tool calls belong to the current
  // assistant message, so they carry acpMsgId to interleave with its text.
  pi.on("tool_execution_start", (ev) => {
    if (!ev?.toolCallId) return;
    if (!acpMsgId) acpMsgId = `m${++acpMsgSeq}`;
    let args = "";
    try {
      args = ev.args === undefined ? "" : JSON.stringify(ev.args);
    } catch {}
    postACP(sock, {
      op: "tool_call",
      messageId: acpMsgId,
      toolCallId: ev.toolCallId,
      toolName: ev.toolName || "",
      kind: kindForToolName(ev.toolName),
      args,
    });
  });

  pi.on("tool_execution_end", (ev) => {
    if (!ev?.toolCallId) return;
    postACP(sock, {
      op: "tool_call_update",
      messageId: acpMsgId,
      toolCallId: ev.toolCallId,
      status: ev.isError ? "failed" : "completed",
      output: toolResultText(ev.result),
    });
  });

  pi.on("agent_end", (ev, ctx) => {
    const msgs = ev.messages ?? [];
    settledMessages = msgs;
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
    // A brand-new session's file exists by now; make sure it's attributed.
    // Reported here rather than at settle so attribution does not wait out a
    // retry sequence.
    reportSession("activity", ctx);
  });

  // The turn boundary. It fires in a `finally` around pi's whole run
  // (agent-session.js _runAgentPrompt), so it is reached for a clean stop, an
  // abort and a thrown/errored run alike.
  pi.on("agent_settled", (_ev, ctx) => {
    if (!runOpen) return; // no run of ours is open (e.g. settled after a rebind)
    runOpen = false;
    const seq = turnSeq;
    heldTrigger = "";
    heldTriggerBytes = 0;
    const msgs = settledMessages;
    settledMessages = [];
    const last = lastAssistant(msgs);
    const outcome = normalizeOutcome(last?.stopReason);
    let name;
    try {
      name = ctx.sessionManager.getSessionName();
    } catch {}
    const title = name || firstUserTitle;
    const ev = {
      op: "turn",
      phase: "end",
      turn_seq: seq,
      outcome,
      title: title || undefined,
    };
    // The latest assistant prose is terminal for this observed span. On a
    // non-completed outcome it is explicitly partial; carrying it is safe
    // because it came from this run's final captured message list.
    const capped = capOutput(assistantProse(last));
    if (capped.text) {
      ev.output = capped.text;
      if (capped.truncated) ev.truncated = true;
    }
    if (outcome === "error") {
      // The account channel, never the result: a short reason, if pi gave one.
      const diag = boundedDiagnostic(last?.errorMessage ?? "");
      if (diag) ev.diagnostic = diag;
    }
    post(sock, ev);
  });
}

// lastAssistant returns the final assistant message of a settled run's message
// list — the one carrying the run's terminal stopReason.
function lastAssistant(msgs) {
  for (let i = (msgs?.length ?? 0) - 1; i >= 0; i--) {
    if (msgs[i]?.role === "assistant") return msgs[i];
  }
  return undefined;
}

// assistantProse extracts what the agent SAID from a pi assistant message: the
// text blocks only. A pi assistant message routinely mixes prose with toolCall
// and thinking blocks, so "the last assistant message" and "the last thing the
// agent said" are different strings — this mirrors the Go renderer
// (packages/adapter/adapters/pi_conversation.go renderPiContent) so a carried
// result and a conversation read agree. A tool-only tail yields "": the turn
// completed with no output, which is reported by OMITTING the field.
export function assistantProse(msg) {
  if (!msg) return "";
  const c = msg.content;
  if (typeof c === "string") return c;
  if (!Array.isArray(c)) return "";
  const parts = [];
  for (const b of c) {
    if (b && b.type === "text" && typeof b.text === "string") {
      parts.push(b.text);
    }
  }
  return parts.join("\n\n");
}

// Caps, applied HERE at the source and sized jointly against every hop (the
// runner's hook body limit and the daemon's SSE scanner limit are sized for the
// worst-case escaped payload of maxOutputBytes).
//
// The invariant: an oversized output NEVER costs the close. The event still
// closes the turn, `truncated` records that the text was cut, and the full text
// remains available through the conversation read.
const maxOutputBytes = 256 * 1024;
const maxDiagnosticBytes = 1024;
const maxUserTextBytes = 8 * 1024;

// capUserText preserves user content byte-for-byte up to a UTF-8-safe cap.
// The ellipsis is an honest source marker and the omitted byte count is carried
// by the runner when whole exchanges must later be dropped.
export function capUserText(s) {
  const text = String(s ?? "");
  const bytes = Buffer.from(text, "utf8");
  if (bytes.length <= maxUserTextBytes) return { text, sourceBytes: bytes.length };
  return { text: decodeWhole(bytes.subarray(0, maxUserTextBytes - 3)) + "…", sourceBytes: bytes.length };
}

// capOutput truncates at a UTF-8 boundary (never mid-rune: a lone surrogate
// would make the close event unserializable, which is exactly the "oversized
// output costs the close" failure).
export function capOutput(s) {
  const bytes = Buffer.from(s ?? "", "utf8");
  if (bytes.length <= maxOutputBytes) return { text: s ?? "", truncated: false };
  return { text: decodeWhole(bytes.subarray(0, maxOutputBytes)), truncated: true };
}

// boundedDiagnostic collapses whitespace and returns a UTF-8-safe, bounded
// account-channel reason. It is never user or assistant report content.
export function boundedDiagnostic(s) {
  const collapsed = String(s ?? "").replace(/[\s\u0085]+/g, " ").trim();
  if (!collapsed) return "";
  const bytes = Buffer.from(collapsed, "utf8");
  if (bytes.length <= maxDiagnosticBytes) return collapsed;
  return decodeWhole(bytes.subarray(0, maxDiagnosticBytes - 3)) + "…";
}

// decodeWhole decodes a byte slice, dropping a trailing partial UTF-8 sequence
// rather than emitting U+FFFD for it.
function decodeWhole(buf) {
  const s = buf.toString("utf8");
  return s.endsWith("\uFFFD") ? s.slice(0, -1) : s;
}

// normalizeOutcome maps pi's StopReason vocabulary onto gmux's stable, agent-
// agnostic outcome vocabulary (ADR 0027). pi's reasons are:
//
//   stop     → completed   the agent finished its own turn
//   aborted  → interrupted pi's explicit abort path (user Esc)
//   error    → error       pi gave up
//   length   → error       the turn was cut off; the answer is not complete
//   toolUse  → error       terminal toolUse is not a finished turn
//   anything else / missing → error
//
// Only pi's ONE explicit abort reason becomes an interruption: interruption is
// durable state that suppresses the completion notification, so an unknown or
// truncated terminal state must not masquerade as an intentional stop. Exported
// for direct unit testing.
export function normalizeOutcome(stopReason) {
  switch (stopReason) {
    case "stop":
      return "completed";
    case "aborted":
      return "interrupted";
    default:
      return "error";
  }
}

// kindForToolName maps a pi tool name to an ACP ToolKind (the semantic category
// the frontend switches on for icon/header/body). Mirrors Go's
// acp.KindForToolName (history path) so the live and durable snapshots agree;
// unknown tools fall back to "other". Translation lives here, at the typed pi
// access point (ADR 0015).
function kindForToolName(name) {
  switch (name) {
    case "bash":
      return "execute";
    case "read":
    case "ls":
      return "read";
    case "edit":
    case "write":
      return "edit";
    case "grep":
    case "find":
    case "glob":
      return "search";
    default:
      return "other";
  }
}


// toolResultText flattens a pi AgentToolResult into plain text for the ACP
// stream. result.content is an array of typed blocks (text/image); only text
// is surfaced. Tolerates a bare string or missing content.
function toolResultText(result) {
  if (!result) return "";
  if (typeof result === "string") return result;
  const c = result.content;
  if (typeof c === "string") return c;
  if (Array.isArray(c)) {
    return c
      .filter((b) => b && b.type === "text" && typeof b.text === "string")
      .map((b) => b.text)
      .join("");
  }
  return "";
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

// --- delivery ---------------------------------------------------------------
// Hook and conversation events are order-sensitive. Keep one chain per channel
// so token deltas cannot overtake one another without coupling hook delivery to
// the higher-volume conversation stream.
const POST_TIMEOUT_MS = 2000;
const DELIVERY_NOTICE_INTERVAL_MS = 5000;

let deliveryChain = Promise.resolve();
let acpChain = Promise.resolve();
let deliveryNotifier;
let lastDeliveryNotice = 0;

function rememberNotifier(ctx) {
  if (typeof ctx?.ui?.notify !== "function") return;
  deliveryNotifier = (message) => ctx.ui.notify(message, "warning");
}

function notifyDeliveryFailure(error) {
  const now = Date.now();
  if (now - lastDeliveryNotice < DELIVERY_NOTICE_INTERVAL_MS) return;
  lastDeliveryNotice = now;
  const detail = error?.code ? `${error.code}: ${error.message}` : String(error);
  const message = `gmux hook unavailable (${detail}); pi will continue`;
  try {
    if (deliveryNotifier) deliveryNotifier(message);
    else console.warn(message);
  } catch {
    // A broken UI or console must not escape the extension either.
  }
}

function postACP(socketPath, event) {
  acpChain = acpChain.then(() => sendOne(socketPath, event, "/acp/ingest", false)).catch(() => {});
}

function sendOne(socketPath, event, path = "/hook/event", notify = true) {
  return new Promise((resolve) => {
    let done = false;
    const settle = () => {
      if (done) return;
      done = true;
      clearTimeout(deadline);
      resolve();
    };
    // The one case "close" cannot cover: a socket that accepts and then never
    // answers or closes must not stall the chain.
    const deadline = setTimeout(settle, POST_TIMEOUT_MS);
    if (typeof deadline.unref === "function") deadline.unref(); // never hold pi open
    try {
      const body = Buffer.from(JSON.stringify(event), "utf8");
      const req = http.request({
        socketPath,
        path,
        method: "POST",
        timeout: POST_TIMEOUT_MS,
        headers: { "content-type": "application/json", "content-length": body.length },
      });
      req.on("response", (res) => {
        if (res.statusCode < 200 || res.statusCode >= 300) {
          const error = new Error(`hook endpoint answered HTTP ${res.statusCode}`);
          error.code = `HTTP_${res.statusCode}`;
          if (notify) notifyDeliveryFailure(error);
        }
        res.resume(); // drain so the socket can close
      });
      req.on("timeout", () => req.destroy());
      // EventEmitter "error" is special: without a listener Node promotes it
      // to uncaughtException even though "close" follows. Consume it, notify
      // best-effort, and settle this delivery link so later hooks still run.
      req.on("error", (error) => {
        if (notify) notifyDeliveryFailure(error);
        settle();
      });
      req.on("close", settle);
      req.end(body);
    } catch {
      settle(); // swallow — the extension must never break pi
    }
  });
}

// post queues one event for in-order delivery. Fire-and-forget: it returns
// immediately and never throws. Exported for delivery-ordering tests.
//
// The terminal catch is load-bearing in two ways: it stops one rejected link
// from poisoning the chain (every later event would skip delivery), and it
// guarantees no unhandled rejection reaches pi's process — node's default is
// to abort on those. sendOne resolves on every known path; the catch exists so
// an unforeseen throw degrades to "this one event was lost" instead of "hook
// reporting stops for the rest of the session".
export function post(socketPath, event) {
  deliveryChain = deliveryChain.then(() => sendOne(socketPath, event)).catch(() => {});
  return deliveryChain;
}
