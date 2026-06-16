// Standalone harness to exercise hook.mjs against a real agent.
//
//   node test-harness.mjs -- pi            # launch pi under node
//   node test-harness.mjs --bun -- pi      # launch pi under bun
//
// It binds a throwaway unix socket (standing in for the runner), sets the
// append-safe env, spawns the agent attached to the current TTY, and prints
// every shim event it receives. Try writing a message, then `/resume` to
// watch the rebind report land.

import http from "node:http";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { unlinkSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";

const here = dirname(fileURLToPath(import.meta.url));
const shim = join(here, "hook.mjs");

const argv = process.argv.slice(2);
const useBun = argv.includes("--bun");
const sepIdx = argv.indexOf("--");
const cmd = sepIdx >= 0 ? argv.slice(sepIdx + 1) : [];
if (cmd.length === 0) {
  console.error("usage: node test-harness.mjs [--bun] -- <agent> [args...]");
  process.exit(2);
}

const sock = join(tmpdir(), `gmux-shim-harness-${process.pid}.sock`);
if (existsSync(sock)) unlinkSync(sock);

const server = http.createServer((req, res) => {
  if (req.method === "POST" && req.url === "/shim/event") {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      try {
        const e = JSON.parse(body);
        const head = `[shim] ${String(e.op).padEnd(7)} pid=${e.pid}`;
        if (e.op === "hello") console.error(`${head} runtime=${e.runtime}`);
        else {
          const preview = e.data ? JSON.stringify(e.data.slice(0, 80)) : "(no inline data)";
          console.error(`${head} bytes=${e.bytes} ${e.path}\n          ${preview}`);
        }
      } catch {
        console.error("[shim] (unparseable)", body);
      }
      res.end("ok");
    });
    return;
  }
  res.statusCode = 404;
  res.end();
});

server.listen(sock, () => {
  console.error(`[harness] listening on ${sock}`);
  console.error(`[harness] launching: ${cmd.join(" ")} (${useBun ? "bun" : "node"})\n`);

  const env = { ...process.env, GMUX_RUNNER_SOCK: sock };
  // Append-safe: preserve any upstream flags.
  if (useBun) {
    env.BUN_OPTIONS = `${process.env.BUN_OPTIONS ?? ""} --preload ${shim}`.trim();
  } else {
    env.NODE_OPTIONS = `${process.env.NODE_OPTIONS ?? ""} --import file://${shim}`.trim();
  }

  const launcher = useBun ? "bun" : cmd[0];
  const args = useBun ? cmd : cmd.slice(1);
  const child = spawn(launcher, args, { stdio: "inherit", env });

  child.on("exit", (code) => {
    server.close();
    try { unlinkSync(sock); } catch {}
    process.exit(code ?? 0);
  });
});
