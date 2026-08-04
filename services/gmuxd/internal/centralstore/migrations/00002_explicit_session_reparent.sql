-- +goose Up

-- ADR 0026 §8 amendment: lifecycle paths still leave launch_parent_id alone,
-- but the explicit domain reparent operation may replace it after validating
-- both retained rows. Keep cycle prevention as a database invariant too.
DROP TRIGGER local_sessions_launch_parent_immutable_update;

-- +goose StatementBegin
CREATE TRIGGER local_sessions_launch_parent_no_cycle_update
BEFORE UPDATE OF launch_parent_id ON local_sessions
WHEN NEW.launch_parent_id IS NOT NULL AND OLD.launch_parent_id IS NOT NEW.launch_parent_id
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

-- +goose Down

DROP TRIGGER local_sessions_launch_parent_no_cycle_update;

-- +goose StatementBegin
CREATE TRIGGER local_sessions_launch_parent_immutable_update
BEFORE UPDATE OF launch_parent_id ON local_sessions
WHEN NEW.launch_parent_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'launch parent can only be cleared');
END;
-- +goose StatementEnd
