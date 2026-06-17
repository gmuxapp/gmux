#!/usr/bin/env node
// Diagnostic harness: launch a command (default `pi`) with trace-hook.mjs
// preloaded, capture every *.jsonl fs op (pid + ms timestamp), and print a
// chronological table on exit. Drive the scenario by hand in the inherited
// terminal (launch → message → response → /resume → select).
//
//   node packages/agent-shim/trace-shim.mjs            # runs `pi`
//   node packages/agent-shim/trace-shim.mjs pi --foo   # runs `pi --foo`
import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const hook = join(here, "trace-hook.mjs");
const logFile = join(mkdtempSync(join(tmpdir(), "shimtrace-")), "events.log");
writeFileSync(logFile, "");

const cmd = process.argv[2] || "pi";
const args = process.argv.slice(3);
const env = {
  ...process.env,
  GMUX_TRACE_LOG: logFile,
  NODE_OPTIONS: `${process.env.NODE_OPTIONS ?? ""} --import file://${hook}`.trim(),
  BUN_OPTIONS: `${process.env.BUN_OPTIONS ?? ""} --preload ${hook}`.trim(),
};

console.error(`[trace] hook:   ${hook}`);
console.error(`[trace] log:    ${logFile}`);
console.error(`[trace] launch: ${cmd} ${args.join(" ")}\n`);

const child = spawn(cmd, args, { stdio: "inherit", env });
child.on("exit", (code) => {
  const lines = readFileSync(logFile, "utf8").trim().split("\n").filter(Boolean);
  const recs = lines.map((l) => JSON.parse(l));
  const pids = [...new Set(recs.map((r) => r.pid))];

  console.error("\n===== *.jsonl fs trace (chronological) =====");
  console.error("    t(ms)   pid     op                  bytes  file");
  for (const r of recs) {
    const base = r.path ? r.path.split("/").pop() : "";
    const extra = r.flags
      ? ` flags=${r.flags}`
      : r.to
        ? ` -> ${String(r.to).split("/").pop()}`
        : r.entries != null
          ? ` entries=${r.entries}`
          : "";
    console.error(
      `${String(r.t).padStart(9)}  ${String(r.pid).padEnd(7)} ${String(r.op).padEnd(18)} ${String(r.bytes ?? "").padStart(6)}  ${base}${extra}`,
    );
  }
  console.error(`\n[trace] ${recs.length} events across ${pids.length} pid(s): ${pids.join(", ")}`);
  console.error(`[trace] raw log: ${logFile}`);
  process.exit(code ?? 0);
});
