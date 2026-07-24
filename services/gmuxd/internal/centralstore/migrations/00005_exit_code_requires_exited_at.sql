-- +goose Up
-- Enforce the durable exit invariant: a row must not store exit_code without
-- exited_at_ms. The invariant was always intended (exit code is meaningless
-- without a timestamp) but was never expressed as a DB-level constraint.
--
-- Remediate any existing rows that violate the invariant before adding the
-- constraint; an existing database that already has malformed rows would
-- otherwise fail the migration entirely. Clearing exit_code (rather than
-- synthesising an exited_at) is the conservative choice: it avoids inventing
-- a timestamp we don't know and lets the normal sweep/registration path
-- re-establish exit state legitimately.
UPDATE local_sessions SET exit_code = NULL
    WHERE exit_code IS NOT NULL AND exited_at_ms IS NULL;

-- SQLite does not support adding a CHECK constraint to an existing column via
-- ALTER TABLE. The canonical workaround is a table rebuild, but that is
-- heavyweight and risky in a migration. A trigger achieves the same forward
-- guarantee without touching the table structure: it fires on any INSERT or
-- UPDATE that would introduce the invariant violation and aborts the statement.
-- +goose StatementBegin
CREATE TRIGGER local_sessions_exit_invariant_insert
    BEFORE INSERT ON local_sessions
    WHEN NEW.exit_code IS NOT NULL AND NEW.exited_at_ms IS NULL
BEGIN
    SELECT RAISE(ABORT, 'exit_code requires exited_at_ms');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER local_sessions_exit_invariant_update
    BEFORE UPDATE ON local_sessions
    WHEN NEW.exit_code IS NOT NULL AND NEW.exited_at_ms IS NULL
BEGIN
    SELECT RAISE(ABORT, 'exit_code requires exited_at_ms');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS local_sessions_exit_invariant_update;
DROP TRIGGER IF EXISTS local_sessions_exit_invariant_insert;
