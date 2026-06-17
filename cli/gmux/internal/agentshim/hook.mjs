// gmux agent shim
// ----------------------------------------------------------------------------
// A tiny, readable preload that gmux injects into node/bun agent processes
// (pi today; any node/bun agent that persists a JSONL session file) so gmux
// learns *authoritatively* which conversation file a runner holds, the
// moment the agent writes it. This replaces post-hoc scrollback matching.
//
// How it gets loaded (set by the gmux runner when it spawns the agent):
//   node:  NODE_OPTIONS="... --import file:///abs/path/hook.mjs"
//   bun:   BUN_OPTIONS="... --preload /abs/path/hook.mjs"
// Both vars are set; each runtime honours its own and ignores the other.
//
// What it does:
//   - Wraps the fs write surface (openSync/appendFileSync/writeFileSync) and,
//     whenever a *.jsonl file is written, POSTs an event to the runner's unix
//     socket: { op, path, pid, data?, bytes }.
//   - Reads are intentionally NOT reported: typing `/resume` makes the agent
//     readdir + bulk-read every session file for its picker, which would be
//     pure noise. A real resume/rebind always ends in a write to the chosen
//     file, so writes alone catch attribution *and* rebind.
//
// Why it's safe in the wrong process:
//   - It arms only when GMUX_RUNNER_SOCK is present, then immediately deletes
//     that var (and our injected *_OPTIONS) from process.env so any child the
//     agent spawns (npm, sub-node, sub-bun) inherits a clean env and disarms
//     itself — even under bun, which re-injects --preload via execArgv.
//   - It is fire-and-forget: the original fs call runs and returns first; the
//     POST is best-effort and never throws back into the agent.
// ----------------------------------------------------------------------------

import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";

const RUNNER_SOCK = process.env.GMUX_RUNNER_SOCK;

// Disarm in every descendant: consume the coordinates and strip the flags
// that point at THIS shim file so children the agent spawns inherit a clean
// env (and don't re-load us). We match the shim's own resolved path
// (import.meta.url) rather than a filename pattern, so the content-addressed
// name the runner materializes (hook-<hash>.mjs) is handled without the
// strip logic drifting from the writer.
delete process.env.GMUX_RUNNER_SOCK;
stripSelfPreload("NODE_OPTIONS");
stripSelfPreload("BUN_OPTIONS");

// No runner to report to → this is a child (or a non-gmux launch). Do nothing.
if (RUNNER_SOCK) {
  install(RUNNER_SOCK);
}

function stripSelfPreload(name) {
  const v = process.env[name];
  if (!v) return;
  let self;
  try {
    self = fileURLToPath(import.meta.url);
  } catch {
    return;
  }
  // Remove any --import/--preload token (optionally file://-prefixed) that
  // references this exact shim path. Both env vars are checked because each
  // runtime is handed both.
  const esc = self.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const re = new RegExp(`\\s*--(?:import|preload)(?:=|\\s+)(?:file://)?${esc}`, "g");
  const next = v.replace(re, "").trim();
  if (next) process.env[name] = next;
  else delete process.env[name];
}

function install(socketPath) {
  const require = createRequire(import.meta.url);
  const fs = require("fs");
  const http = require("http");

  // Tracks fd -> path so writeFileSync(fd, ...) can be attributed. pi's
  // first flush is openSync(path,"wx") + writeFileSync(fd, line) per entry.
  const fdPaths = new Map();
  const pid = process.pid;

  // Node implements appendFileSync via writeFileSync({flag:"a"}); without
  // this guard our append wrapper re-enters our write wrapper and the same
  // delta is reported twice. Bun's appendFileSync is native (no re-entry).
  let reporting = false;

  const isSession = (p) => typeof p === "string" && p.endsWith(".jsonl");

  // A read-open of an existing session file is the agent *binding* to a
  // conversation (a /resume selection, or a `--session X` launch): it reads
  // but doesn't write, so the write hooks never fire. Reporting it lets the
  // runner attribute on bind, not only on the first subsequent write.
  // O_RDONLY is 0, so numeric flags are read-open when no write bit is set;
  // string flags starting with "r" (r, rs, r+) are a read/bind.
  const isReadOpen = (flags) => {
    if (flags == null) return true; // openSync default is "r"
    if (typeof flags === "number") return (flags & 3) === 0;
    return String(flags).startsWith("r");
  };

  function report(op, path, data) {
    let bytes = 0;
    let text;
    if (data != null) {
      const buf = Buffer.isBuffer(data) ? data : Buffer.from(String(data), "utf8");
      bytes = buf.length;
      // Cap inline payload; daemon re-reads if it needs more than this.
      if (bytes <= 256 * 1024) text = buf.toString("utf8");
    }
    post({ op, path, pid, bytes, data: text });
  }

  function post(event) {
    try {
      const body = Buffer.from(JSON.stringify(event), "utf8");
      const req = http.request({
        socketPath,
        path: "/shim/event",
        method: "POST",
        headers: { "content-type": "application/json", "content-length": body.length },
      });
      req.on("error", () => {}); // never surface transport errors into the agent
      req.end(body);
    } catch {
      // swallow — the shim must never break the agent
    }
  }

  // --- wrap openSync: remember which path an fd points at ---
  // Reading a single session file always starts by opening a handle to it,
  // whether sync, async, or promise-based. Hooking the whole open family
  // means we attribute a bind regardless of which one the agent picks, so we
  // don't couple to pi using openSync specifically (a sync→async change
  // wouldn't break us). The subsequent read (readSync, handle.read, ...) is
  // implied by the open and needs no separate hook. Bulk/stream reads
  // (createReadStream, the /resume picker's list scan) are deliberately NOT
  // hooked: those touch many files and would churn attribution.
  const noteReadOpen = (path, flags) => {
    if (isSession(path) && isReadOpen(flags)) report("read", String(path));
  };

  const origOpenSync = fs.openSync;
  fs.openSync = function (path, flags, ...rest) {
    const fd = origOpenSync.call(this, path, flags, ...rest);
    if (isSession(path)) {
      fdPaths.set(fd, String(path));
      noteReadOpen(path, flags);
    }
    return fd;
  };

  // Async callback form: fs.open(path[, flags[, mode]], cb). When flags is
  // omitted the next arg is the callback and the default mode is "r".
  const origOpen = fs.open;
  if (typeof origOpen === "function") {
    fs.open = function (path, ...rest) {
      const flags = typeof rest[0] === "function" ? undefined : rest[0];
      noteReadOpen(path, flags);
      return origOpen.call(this, path, ...rest);
    };
  }

  // Promise form: fs.promises.open(path[, flags[, mode]]) → FileHandle.
  if (fs.promises && typeof fs.promises.open === "function") {
    const origPOpen = fs.promises.open;
    fs.promises.open = function (path, flags, ...rest) {
      noteReadOpen(path, flags);
      return origPOpen.call(this, path, flags, ...rest);
    };
  }

  const origCloseSync = fs.closeSync;
  fs.closeSync = function (fd, ...rest) {
    fdPaths.delete(fd);
    return origCloseSync.call(this, fd, ...rest);
  };

  // --- wrap appendFileSync: the per-line delta path ---
  const origAppend = fs.appendFileSync;
  fs.appendFileSync = function (path, data, ...rest) {
    const outer = !reporting;
    reporting = true;
    try {
      const r = origAppend.call(this, path, data, ...rest);
      const p = typeof path === "number" ? fdPaths.get(path) : path;
      if (outer && isSession(p)) report("append", p, data);
      return r;
    } finally {
      if (outer) reporting = false;
    }
  };

  // --- wrap writeFileSync: full rewrites + fd-based flush ---
  const origWrite = fs.writeFileSync;
  fs.writeFileSync = function (path, data, ...rest) {
    const r = origWrite.call(this, path, data, ...rest);
    const p = typeof path === "number" ? fdPaths.get(path) : path;
    if (!reporting && isSession(p)) report("write", p, data);
    return r;
  };

  // Announce ourselves so the runner knows a shim is live for this pid
  // even before the first session write (useful for "is this shimmed?").
  post({ op: "hello", pid, runtime: typeof Bun !== "undefined" ? "bun" : "node" });
}
