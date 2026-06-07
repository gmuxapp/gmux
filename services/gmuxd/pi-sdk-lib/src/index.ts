#!/usr/bin/env node
/**
 * pi-sdk-lib — subprocess bridge between gmuxd and the pi SDK.
 *
 * Spawned by the pi-sdk gmuxd adapter. Owns one AgentSession for its
 * lifetime. Communicates via newline-delimited JSON on stdin/stdout.
 *
 * Stdin  (gmuxd → Node):
 *   { type: "prompt", text: string, images?: ImageContent[] }
 *   { type: "abort" }
 *
 * Stdout (Node → gmuxd):
 *   { type: "session_ready", sessionId, sessionFile, model, thinkingLevel }
 *   ... AgentSessionEvents forwarded verbatim ...
 *   { type: "error", message: string }           // fatal
 *   { type: "protocol_error", line: string }     // bad stdin line, non-fatal
 */

import * as readline from "node:readline";
import {
  AuthStorage,
  ModelRegistry,
  SessionManager,
  createAgentSession,
  type AgentSessionEvent,
  type PromptOptions,
} from "@earendil-works/pi-coding-agent";

// ---------------------------------------------------------------------------
// Argv parsing
// ---------------------------------------------------------------------------

interface Args {
  cwd: string;
  sessionFile: string | undefined;
  model: string | undefined;
  thinking: string | undefined;
}

function parseArgs(): Args {
  const argv = process.argv.slice(2);
  const args: Args = {
    cwd: process.cwd(),
    sessionFile: undefined,
    model: undefined,
    thinking: undefined,
  };
  for (let i = 0; i < argv.length; i++) {
    switch (argv[i]) {
      case "--cwd":
        args.cwd = argv[++i] ?? args.cwd;
        break;
      case "--session":
        args.sessionFile = argv[++i];
        break;
      case "--model":
        args.model = argv[++i];
        break;
      case "--thinking":
        args.thinking = argv[++i];
        break;
    }
  }
  return args;
}

// ---------------------------------------------------------------------------
// Stdout helpers
// ---------------------------------------------------------------------------

function emit(obj: unknown): void {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

async function main(): Promise<void> {
  const args = parseArgs();

  // Build SessionManager
  const sessionManager = args.sessionFile
    ? SessionManager.open(args.sessionFile)
    : SessionManager.create(args.cwd);

  // Resolve model object from --model flag ("provider/modelId" format)
  type ThinkingLevel = "off" | "minimal" | "low" | "medium" | "high" | "xhigh";
  const validLevels: ThinkingLevel[] = [
    "off", "minimal", "low", "medium", "high", "xhigh",
  ];

  type ModelArg = NonNullable<Parameters<typeof createAgentSession>[0]>["model"];
  let modelObj: ModelArg = undefined;

  if (args.model) {
    const slash = args.model.indexOf("/");
    if (slash > 0) {
      const provider = args.model.slice(0, slash);
      const modelId = args.model.slice(slash + 1);
      const auth = AuthStorage.create();
      const registry = ModelRegistry.create(auth);
      modelObj = registry.find(provider, modelId) ?? undefined;
    }
  }

  const thinkingLevel = validLevels.includes(args.thinking as ThinkingLevel)
    ? (args.thinking as ThinkingLevel)
    : undefined;

  // Create session (extensions load by default, same as interactive mode)
  const { session } = await createAgentSession({
    cwd: args.cwd,
    model: modelObj,
    thinkingLevel,
    sessionManager,
  });

  // Guard: model must be resolved
  if (!session.model) {
    emit({ type: "error", message: "no model available" });
    process.exit(1);
  }

  // Subscribe — forward all events verbatim
  session.subscribe((event: AgentSessionEvent) => {
    emit(event);
  });

  // Emit startup event
  emit({
    type: "session_ready",
    sessionId: session.sessionId,
    sessionFile: session.sessionFile,
    model: session.model.id,
    thinkingLevel: session.thinkingLevel,
  });

  // ---------------------------------------------------------------------------
  // Stdin dispatch loop
  // ---------------------------------------------------------------------------

  const rl = readline.createInterface({
    input: process.stdin,
    crlfDelay: Infinity,
  });

  rl.on("line", (line) => {
    const trimmed = line.trim();
    if (!trimmed) return;

    let msg: { type: string; text?: string; images?: unknown[] };
    try {
      msg = JSON.parse(trimmed) as { type: string; text?: string; images?: unknown[] };
    } catch {
      emit({ type: "protocol_error", line: trimmed });
      return;
    }

    switch (msg.type) {
      case "prompt": {
        const text = msg.text ?? "";
        // images are forwarded as-is from JSON; SDK accepts ImageContent[]
        const options: PromptOptions = {};
        if (msg.images) {
          options.images = msg.images as PromptOptions["images"];
        }
        if (session.isStreaming) {
          options.streamingBehavior = "steer";
        }
        session.prompt(text, options).catch(onError);
        break;
      }
      case "abort":
        session.abort().catch(onError);
        break;
      default:
        emit({ type: "protocol_error", line: trimmed });
    }
  });

  // Graceful shutdown: abort in-flight work before disposing
  rl.on("close", () => {
    session
      .abort()
      .then(() => {
        session.dispose();
        process.exit(0);
      })
      .catch(() => {
        session.dispose();
        process.exit(0);
      });
  });
}

// ---------------------------------------------------------------------------
// Error handling
// ---------------------------------------------------------------------------

function onError(err: unknown): void {
  const message = err instanceof Error ? err.message : String(err);
  emit({ type: "error", message });
}

process.on("unhandledRejection", (reason) => {
  onError(reason);
  process.exit(1);
});

main().catch((err) => {
  onError(err);
  process.exit(1);
});
