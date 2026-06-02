/**
 * gmux_verify — browser verification tool for james-gmux development.
 *
 * Selects the right daemon and app port for the scenario, starts the dev
 * stack if needed, fetches the auth token, finds the project slug from
 * projects.json, authenticates the browser, and navigates to the requested
 * route.
 *
 * Three scenarios:
 *   frontend — prod daemon (:8790) + vite (:5173). Use for UI/CSS changes.
 *   full     — dev daemon (:22226) + vite (:5173). Use when Go code changed.
 *   prod     — prod daemon (:8790), no vite. Use to reproduce bugs or verify
 *              after `just install`.
 */

import { execSync, spawn } from "node:child_process";
import { readFileSync, readdirSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";

// ── scenario config ────────────────────────────────────────────────────────────

const SCENARIOS = {
	frontend: {
		daemonPort: 8790,
		appPort: 5173,
		tokenGlob: "gmux",
		tokenExclude: "gmux-dev" as string | null,
		startCmd: "just dev-frontend" as string | null,
		daemonTimeoutMs: 20_000,
	},
	full: {
		daemonPort: 22226,
		appPort: 5173,
		tokenGlob: "gmux-dev",
		tokenExclude: null as string | null,
		startCmd: "just dev" as string | null,
		daemonTimeoutMs: 60_000, // includes Go build time
	},
	prod: {
		daemonPort: 8790,
		appPort: 8790,
		tokenGlob: "gmux",
		tokenExclude: "gmux-dev" as string | null,
		startCmd: null as string | null,
		daemonTimeoutMs: 0,
	},
} as const;

type Scenario = keyof typeof SCENARIOS;

// ── helpers ────────────────────────────────────────────────────────────────────

function sleep(ms: number): Promise<void> {
	return new Promise((r) => setTimeout(r, ms));
}

function isDaemonUp(port: number): boolean {
	try {
		execSync(`curl -sf http://localhost:${port}/v1/health -o /dev/null`, {
			timeout: 2000,
			stdio: "pipe",
		});
		return true;
	} catch {
		return false;
	}
}

function isViteUp(): boolean {
	try {
		execSync("curl -sf http://localhost:5173 -o /dev/null", {
			timeout: 2000,
			stdio: "pipe",
		});
		return true;
	} catch {
		return false;
	}
}

async function pollUntil(
	check: () => boolean,
	timeoutMs: number,
	intervalMs = 500,
): Promise<boolean> {
	const deadline = Date.now() + timeoutMs;
	while (Date.now() < deadline) {
		if (check()) return true;
		await sleep(intervalMs);
	}
	return false;
}

function spawnBackground(cmd: string, cwd: string): void {
	const proc = spawn("bash", ["-c", cmd], {
		cwd,
		detached: true,
		stdio: "ignore",
	});
	proc.unref();
}

function sh(cmd: string): string {
	return execSync(cmd, { encoding: "utf-8", stdio: "pipe" }).trim();
}

// ── state dir + token ──────────────────────────────────────────────────────────

function findStateDir(
	tokenGlob: string,
	tokenExclude: string | null,
): string | null {
	const base = join(homedir(), ".local", "state");
	let entries: string[];
	try {
		entries = readdirSync(base);
	} catch {
		return null;
	}
	const match = entries
		.filter((d) => d.includes(tokenGlob))
		.filter((d) => !tokenExclude || !d.includes(tokenExclude))
		.map((d) => join(base, d));
	return match[0] ?? null;
}

function readToken(stateDir: string): string {
	try {
		return readFileSync(join(stateDir, "auth-token"), "utf-8").trim();
	} catch {
		return "";
	}
}

// ── slug lookup ────────────────────────────────────────────────────────────────

interface ProjectsState {
	version: number;
	items: Array<{
		slug: string;
		match: Array<{ path?: string; remote?: string; exact?: boolean }>;
	}>;
}

function findSlug(stateDir: string, repoRoot: string): string {
	let state: ProjectsState;
	try {
		state = JSON.parse(readFileSync(join(stateDir, "projects.json"), "utf-8"));
	} catch {
		return "";
	}

	let bestSlug = "";
	let bestLen = 0;

	for (const item of state.items) {
		for (const rule of item.match) {
			if (!rule.path) continue;
			// Expand ~ to home dir to match NormalizePath behaviour in Go.
			const norm = rule.path.startsWith("~")
				? homedir() + rule.path.slice(1)
				: rule.path;
			if (rule.exact) {
				if (repoRoot === norm && norm.length > bestLen) {
					bestSlug = item.slug;
					bestLen = norm.length;
				}
			} else {
				if (
					(repoRoot === norm || repoRoot.startsWith(norm + "/")) &&
					norm.length > bestLen
				) {
					bestSlug = item.slug;
					bestLen = norm.length;
				}
			}
		}
	}

	return bestSlug;
}

// ── extension ──────────────────────────────────────────────────────────────────

export default function (pi: ExtensionAPI) {
	pi.registerTool({
		name: "gmux_verify",
		label: "gmux verify",
		description: [
			"Open a james-gmux app route in the browser for the given dev scenario.",
			"Starts the dev stack if not running, finds the project slug from projects.json,",
			"authenticates, and navigates.",
			"scenario: 'frontend' (UI/CSS — prod daemon :8790 + vite :5173),",
			"'full' (Go changed — dev daemon :22226 + vite :5173),",
			"'prod' (bug repro or post-install verify — prod daemon :8790 only).",
			"route is the path after the slug, e.g. 'sessions' or '' for the project home.",
		].join(" "),
		promptGuidelines: [
			"Always use gmux_verify instead of manually constructing TOKEN= commands or agent-browser navigate calls.",
			"frontend: UI/React/CSS only, no Go changes. full: any .go file changed. prod: reproduce a bug or verify after just install.",
		],
		parameters: Type.Object({
			scenario: Type.Union(
				[
					Type.Literal("frontend"),
					Type.Literal("full"),
					Type.Literal("prod"),
				],
				{ description: "Which dev environment to use" },
			),
			route: Type.String({
				description:
					"Path after the project slug, e.g. 'sessions' or '' for the project home. No leading slash needed.",
			}),
			screenshotPath: Type.Optional(
				Type.String({
					description:
						"Absolute path to save a PNG screenshot after navigating. Omit to skip.",
				}),
			),
		}),

		async execute(_id, params, _signal, _onUpdate, ctx) {
			const cfg = SCENARIOS[params.scenario as Scenario];
			const repoRoot = ctx.cwd;
			const lines: string[] = [];
			const ok = (msg: string) => lines.push(`✓ ${msg}`);
			const info = (msg: string) => lines.push(`  ${msg}`);
			const fail = (msg: string) => ({
				content: [
					{ type: "text" as const, text: [...lines, `✗ ${msg}`].join("\n") },
				],
				details: {},
				isError: true,
			});

			// ── 1. Daemon ─────────────────────────────────────────────────────────

			let justStarted = false;

			if (!isDaemonUp(cfg.daemonPort)) {
				if (!cfg.startCmd) {
					return fail(
						`Prod daemon not running on :${cfg.daemonPort}. ` +
							`It should already be running — run \`gmuxd status\` to check.`,
					);
				}
				info(`Daemon not running. Starting: ${cfg.startCmd}`);
				spawnBackground(cfg.startCmd, repoRoot);
				justStarted = true;
				info(
					`Waiting for daemon on :${cfg.daemonPort} (up to ${cfg.daemonTimeoutMs / 1000}s)…`,
				);
				const ready = await pollUntil(
					() => isDaemonUp(cfg.daemonPort),
					cfg.daemonTimeoutMs,
				);
				if (!ready) {
					return fail(
						`Daemon on :${cfg.daemonPort} did not start within ${cfg.daemonTimeoutMs / 1000}s. ` +
							`Run \`${cfg.startCmd}\` manually to see errors.`,
					);
				}
				ok(`Daemon up on :${cfg.daemonPort}`);
			} else {
				ok(`Daemon up on :${cfg.daemonPort}`);
			}

			// ── 2. Vite (frontend + full only) ────────────────────────────────────

			if (cfg.appPort === 5173) {
				if (!isViteUp()) {
					if (params.scenario === "frontend" && !justStarted) {
						// Daemon was already up but vite wasn't started — start it separately.
						info("Vite not running. Starting: just dev-frontend");
						spawnBackground("just dev-frontend", repoRoot);
					}
					// For 'full', just dev (already launched above) also starts vite.
					// In either case, wait for it.
					info("Waiting for vite on :5173 (up to 20s)…");
					const viteReady = await pollUntil(() => isViteUp(), 20_000);
					if (!viteReady) {
						return fail(
							"Vite did not come up on :5173 within 20s. " +
								"Check the dev stack output for errors.",
						);
					}
					ok("Vite up on :5173");
				} else {
					ok("Vite up on :5173");
				}
			}

			// ── 3. State dir, token, slug ─────────────────────────────────────────

			const stateDir = findStateDir(cfg.tokenGlob, cfg.tokenExclude);
			if (!stateDir) {
				return fail(
					`No state dir matching '${cfg.tokenGlob}'` +
						(cfg.tokenExclude ? ` (excluding '${cfg.tokenExclude}')` : "") +
						` found under ~/.local/state/. Is the daemon running?`,
				);
			}

			const token = readToken(stateDir);
			if (!token) {
				return fail(`No auth-token in ${stateDir}.`);
			}
			ok("Token found");

			const slug = findSlug(stateDir, repoRoot);
			if (!slug) {
				return fail(
					`No project matching path '${repoRoot}' in ${stateDir}/projects.json. ` +
						`Open the gmux UI and add this directory as a project first.`,
				);
			}
			ok(`Project slug: ${slug}`);

			// ── 4. Auth + navigate ────────────────────────────────────────────────

			const baseUrl = `http://localhost:${cfg.appPort}`;
			const routeClean = params.route.replace(/^\//, "");
			const appUrl = routeClean
				? `${baseUrl}/${slug}/${routeClean}`
				: `${baseUrl}/${slug}`;

			try {
				info(`Auth → ${baseUrl}/auth/login`);
				sh(`agent-browser navigate "${baseUrl}/auth/login?token=${token}"`);
			} catch (e) {
				return fail(`agent-browser navigate (auth) failed: ${e}`);
			}

			try {
				info(`Navigate → ${appUrl}`);
				sh(`agent-browser navigate "${appUrl}"`);
			} catch (e) {
				return fail(`agent-browser navigate (route) failed: ${e}`);
			}
			ok(`At ${appUrl}`);

			// ── 5. Screenshot (optional) ──────────────────────────────────────────

			if (params.screenshotPath) {
				try {
					sh(`agent-browser screenshot "${params.screenshotPath}"`);
					ok(`Screenshot → ${params.screenshotPath}`);
				} catch (e) {
					info(`Screenshot failed (non-fatal): ${e}`);
				}
			}

			// ── 6. Return ─────────────────────────────────────────────────────────

			return {
				content: [
					{
						type: "text" as const,
						text: [
							...lines,
							"",
							`URL:      ${appUrl}`,
							`Scenario: ${params.scenario}  daemon :${cfg.daemonPort}  app :${cfg.appPort}`,
						].join("\n"),
					},
				],
				details: {
					scenario: params.scenario,
					url: appUrl,
					slug,
					daemonPort: cfg.daemonPort,
					appPort: cfg.appPort,
					screenshotPath: params.screenshotPath ?? null,
				},
			};
		},
	});
}
