-- +goose Up

-- Drive mode (ADR 0033): how gmux hosts this harness — 'terminal' (the PTY
-- runner) or 'acp' (the terminal-less ACP runner). Durable because a
-- retained session resumes in the mode it was registered in; it changes
-- only through the explicit relaunch-conversion operation. Every session
-- registered before this migration was a terminal session, so the backfill
-- default is exact, not a guess.
ALTER TABLE local_sessions
    ADD COLUMN drive_mode TEXT NOT NULL DEFAULT 'terminal'
        CHECK (drive_mode IN ('terminal', 'acp'));
