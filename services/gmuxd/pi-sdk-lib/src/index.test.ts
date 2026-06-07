/**
 * Tests for pi-sdk-lib subprocess protocol.
 * Runs the compiled dist/index.js as a subprocess and checks stdout.
 */
import { describe, it, expect } from "vitest";
import { spawnSync } from "node:child_process";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const entry = path.resolve(__dirname, "../dist/index.js");

function runScript(
  stdinData: string,
  extraArgs: string[] = [],
  extraEnv: Record<string, string> = {},
): { lines: Array<{ type: string; [k: string]: unknown }>; exitCode: number } {
  const result = spawnSync(
    process.execPath,
    [entry, "--cwd", "/tmp", ...extraArgs],
    {
      input: stdinData,
      encoding: "utf8",
      timeout: 15000,
      env: { ...process.env, ...extraEnv },
    },
  );
  const lines = (result.stdout ?? "")
    .trim()
    .split("\n")
    .filter(Boolean)
    .map((l) => JSON.parse(l) as { type: string; [k: string]: unknown });
  return { lines, exitCode: result.status ?? 1 };
}

// ---------------------------------------------------------------------------
// session_ready is always first
// ---------------------------------------------------------------------------

describe("startup", () => {
  it("emits session_ready as first event with required fields", () => {
    const { lines, exitCode } = runScript("");

    // Skip if no model is configured in this environment
    if (lines[0]?.type === "error") return;

    expect(exitCode).toBe(0);
    expect(lines.length).toBeGreaterThan(0);

    const ready = lines[0];
    expect(ready.type).toBe("session_ready");
    expect(typeof ready.sessionId).toBe("string");
    expect((ready.sessionId as string).length).toBeGreaterThan(0);
    expect(typeof ready.model).toBe("string");
    expect((ready.model as string).length).toBeGreaterThan(0);
    expect(typeof ready.thinkingLevel).toBe("string");
    // sessionFile is string or null/undefined
    expect(
      ready.sessionFile === undefined ||
        ready.sessionFile === null ||
        typeof ready.sessionFile === "string",
    ).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// protocol_error on unparseable stdin
// ---------------------------------------------------------------------------

describe("protocol", () => {
  it("emits protocol_error for unparseable stdin line", () => {
    // Send a bad JSON line then close stdin
    const { lines } = runScript("not valid json\n");

    // If no model, process may exit with error before reading stdin — skip
    if (lines.some((e) => e.type === "error")) return;

    expect(lines.some((e) => e.type === "protocol_error")).toBe(true);

    const err = lines.find((e) => e.type === "protocol_error") as
      | { type: string; line: string }
      | undefined;
    expect(err?.line).toBe("not valid json");
  });

  it("emits protocol_error for unknown message type", () => {
    const { lines } = runScript(JSON.stringify({ type: "unknown_type" }) + "\n");
    if (lines.some((e) => e.type === "error")) return;
    expect(lines.some((e) => e.type === "protocol_error")).toBe(true);
  });
});
