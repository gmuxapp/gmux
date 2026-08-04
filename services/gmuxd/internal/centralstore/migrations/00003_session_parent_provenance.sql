-- +goose Up

-- ADR 0026 §8 amendment. The v1 launch_parent_id column carried the behavioral
-- parent edge. Rename it to match that role, then preserve the best available
-- launch provenance for existing rows by copying their current parent.
ALTER TABLE local_sessions RENAME COLUMN launch_parent_id TO parent_session_id;
ALTER TABLE local_sessions ADD COLUMN launched_from_session_id TEXT;
UPDATE local_sessions SET launched_from_session_id = parent_session_id;

DROP TRIGGER local_sessions_launch_parent_no_cycle_insert;
DROP TRIGGER local_sessions_launch_parent_immutable_update;

-- +goose StatementBegin
CREATE TRIGGER local_sessions_parent_no_cycle_insert
BEFORE INSERT ON local_sessions
WHEN NEW.parent_session_id IS NOT NULL
BEGIN
    WITH RECURSIVE ancestors(id) AS (
        SELECT NEW.parent_session_id
        UNION
        SELECT s.parent_session_id
        FROM local_sessions AS s
        JOIN ancestors AS a ON s.id = a.id
        WHERE s.parent_session_id IS NOT NULL
    )
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM ancestors WHERE id = NEW.id
    ) THEN RAISE(ABORT, 'session parent cycle') END;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER local_sessions_parent_no_cycle_update
BEFORE UPDATE OF parent_session_id ON local_sessions
WHEN NEW.parent_session_id IS NOT NULL AND OLD.parent_session_id IS NOT NEW.parent_session_id
BEGIN
    WITH RECURSIVE ancestors(id) AS (
        SELECT NEW.parent_session_id
        UNION
        SELECT s.parent_session_id
        FROM local_sessions AS s
        JOIN ancestors AS a ON s.id = a.id
        WHERE s.parent_session_id IS NOT NULL
    )
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM ancestors WHERE id = NEW.id
    ) THEN RAISE(ABORT, 'session parent cycle') END;
END;
-- +goose StatementEnd

-- Launch provenance is captured once on insert and survives every parent
-- mutation and deletion repair.
-- +goose StatementBegin
CREATE TRIGGER local_sessions_launched_from_immutable_update
BEFORE UPDATE OF launched_from_session_id ON local_sessions
WHEN OLD.launched_from_session_id IS NOT NEW.launched_from_session_id
BEGIN
    SELECT RAISE(ABORT, 'launched-from session is immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER local_sessions_launched_from_valid_insert
BEFORE INSERT ON local_sessions
WHEN NEW.launched_from_session_id IS NOT NULL AND
     (length(NEW.launched_from_session_id) = 0 OR NEW.launched_from_session_id = NEW.id)
BEGIN
    SELECT RAISE(ABORT, 'invalid launched-from session');
END;
-- +goose StatementEnd

-- +goose Down

DROP TRIGGER local_sessions_launched_from_valid_insert;
DROP TRIGGER local_sessions_launched_from_immutable_update;
DROP TRIGGER local_sessions_parent_no_cycle_update;
DROP TRIGGER local_sessions_parent_no_cycle_insert;

ALTER TABLE local_sessions DROP COLUMN launched_from_session_id;
ALTER TABLE local_sessions RENAME COLUMN parent_session_id TO launch_parent_id;

-- +goose StatementBegin
CREATE TRIGGER local_sessions_launch_parent_no_cycle_insert
BEFORE INSERT ON local_sessions
WHEN NEW.launch_parent_id IS NOT NULL
BEGIN
    WITH RECURSIVE ancestors(id) AS (
        SELECT NEW.launch_parent_id
        UNION
        SELECT s.launch_parent_id
        FROM local_sessions AS s
        JOIN ancestors AS a ON s.id = a.id
        WHERE s.launch_parent_id IS NOT NULL
    )
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM ancestors WHERE id = NEW.id
    ) THEN RAISE(ABORT, 'launch parent cycle') END;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER local_sessions_launch_parent_immutable_update
BEFORE UPDATE OF launch_parent_id ON local_sessions
WHEN NEW.launch_parent_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'launch parent can only be cleared');
END;
-- +goose StatementEnd
