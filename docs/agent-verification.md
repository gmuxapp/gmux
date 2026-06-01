# Agent Manual Verification

Pick a setup based on what changed, load the skill, and follow it.

| Scenario | Skill |
|---|---|
| UI / React / CSS only; no Go changes | `skills/dev-frontend` |
| Go daemon code changed | `skills/dev-full` |
| Confirm a bug exists in production | `skills/dev-prod` |

**Default is `dev-frontend`.** Use `dev-full` only when Go source has changed.
Use `dev-prod` to confirm a bug in production before starting a fix.

For automated E2E tests, see [docs/e2e.md](e2e.md) — the Playwright suite manages
its own isolated daemon and does not use any of the setups above.
