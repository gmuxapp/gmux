// fs trace hook (diagnostic only — NOT the production shim).
//
// Preloaded into pi/node/bun to record every fs operation touching a
// *.jsonl path, with a per-process pid and a millisecond timestamp, to the
// file named by GMUX_TRACE_LOG. Unlike the production shim (writes only,
// via socket), this also captures reads/readdir/rename and async + promise
// + stream APIs, so we can see the full launch → message → /resume → select
// lifecycle and spot writes that happen on an unhooked path or in a child.
//
//   node: NODE_OPTIONS="--import file:///abs/trace-hook.mjs"
//   bun:  BUN_OPTIONS="--preload /abs/trace-hook.mjs"
import { createRequire } from "node:module";

const LOG = process.env.GMUX_TRACE_LOG;
if (LOG) install();

function install() {
  const require = createRequire(import.meta.url);
  const fs = require("fs");
  const pid = process.pid;
  const t0 = Date.now();
  const fdPaths = new Map();
  let guard = false; // prevents our own log writes from re-entering wrappers

  const isSession = (p) => typeof p === "string" && p.endsWith(".jsonl");
  const looksSessionDir = (p) =>
    typeof p === "string" && /session|\.pi|\.claude|\.codex/i.test(p);

  function log(op, path, extra) {
    if (guard) return;
    guard = true;
    try {
      const rec = { t: Date.now() - t0, pid, op, ...extra };
      if (path != null) rec.path = String(path);
      fs.appendFileSync(LOG, JSON.stringify(rec) + "\n");
    } catch {
      // never break the agent
    } finally {
      guard = false;
    }
  }

  const wrap = (obj, name, make) => {
    const orig = obj?.[name];
    if (typeof orig === "function") obj[name] = make(orig);
  };
  const bytesOf = (d) => {
    try {
      return Buffer.isBuffer(d) ? d.length : Buffer.byteLength(String(d ?? ""));
    } catch {
      return 0;
    }
  };

  log("hello", undefined, { runtime: typeof Bun !== "undefined" ? "bun" : "node" });

  // --- fd bookkeeping ---
  wrap(fs, "openSync", (o) => function (path, flags, ...r) {
    const fd = o.call(this, path, flags, ...r);
    if (isSession(path)) {
      fdPaths.set(fd, String(path));
      log("openSync", path, { flags: String(flags ?? "r"), fd });
    }
    return fd;
  });
  wrap(fs, "closeSync", (o) => function (fd, ...r) {
    fdPaths.delete(fd);
    return o.call(this, fd, ...r);
  });

  // --- sync writes ---
  wrap(fs, "appendFileSync", (o) => function (path, data, ...r) {
    const p = typeof path === "number" ? fdPaths.get(path) : path;
    const res = o.call(this, path, data, ...r);
    if (isSession(p)) log("appendFileSync", p, { bytes: bytesOf(data) });
    return res;
  });
  wrap(fs, "writeFileSync", (o) => function (path, data, ...r) {
    const p = typeof path === "number" ? fdPaths.get(path) : path;
    const res = o.call(this, path, data, ...r);
    if (isSession(p)) log("writeFileSync", p, { bytes: bytesOf(data) });
    return res;
  });
  wrap(fs, "writeSync", (o) => function (fd, ...r) {
    const p = typeof fd === "number" ? fdPaths.get(fd) : undefined;
    const res = o.call(this, fd, ...r);
    if (isSession(p)) log("writeSync(fd)", p, {});
    return res;
  });

  // --- sync reads ---
  wrap(fs, "readFileSync", (o) => function (path, ...r) {
    if (isSession(path)) log("readFileSync", path, {});
    return o.call(this, path, ...r);
  });
  wrap(fs, "readSync", (o) => function (fd, ...r) {
    const p = typeof fd === "number" ? fdPaths.get(fd) : undefined;
    if (isSession(p)) log("readSync(fd)", p, {});
    return o.call(this, fd, ...r);
  });
  wrap(fs, "readdirSync", (o) => function (path, ...r) {
    const res = o.call(this, path, ...r);
    if (looksSessionDir(path)) {
      log("readdirSync", path, { entries: Array.isArray(res) ? res.length : undefined });
    }
    return res;
  });
  wrap(fs, "renameSync", (o) => function (a, b, ...r) {
    if (isSession(a) || isSession(b)) log("renameSync", a, { to: String(b) });
    return o.call(this, a, b, ...r);
  });

  // --- async + stream surfaces the production shim does NOT hook ---
  wrap(fs, "writeFile", (o) => function (path, ...r) {
    if (isSession(path)) log("writeFile(async)", path, {});
    return o.call(this, path, ...r);
  });
  wrap(fs, "appendFile", (o) => function (path, ...r) {
    if (isSession(path)) log("appendFile(async)", path, {});
    return o.call(this, path, ...r);
  });
  wrap(fs, "createWriteStream", (o) => function (path, ...r) {
    if (isSession(path)) log("createWriteStream", path, {});
    return o.call(this, path, ...r);
  });
  if (fs.promises) {
    wrap(fs.promises, "writeFile", (o) => function (path, ...r) {
      if (isSession(path)) log("promises.writeFile", path, {});
      return o.call(this, path, ...r);
    });
    wrap(fs.promises, "appendFile", (o) => function (path, ...r) {
      if (isSession(path)) log("promises.appendFile", path, {});
      return o.call(this, path, ...r);
    });
    wrap(fs.promises, "readFile", (o) => function (path, ...r) {
      if (isSession(path)) log("promises.readFile", path, {});
      return o.call(this, path, ...r);
    });
  }
}
