-- +goose Up
-- Presentation promotion becomes structural promotion: preserve user intent.
UPDATE local_sessions SET parent_session_id = NULL WHERE promoted_to_root = 1;
ALTER TABLE local_sessions DROP COLUMN promoted_to_root;

-- +goose Down
ALTER TABLE local_sessions ADD COLUMN promoted_to_root INTEGER NOT NULL DEFAULT 0 CHECK (promoted_to_root IN (0, 1));
