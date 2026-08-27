-- +goose Up
-- Presentation promotion becomes structural promotion: preserve user intent.
UPDATE local_sessions SET parent_session_id = NULL WHERE promoted_to_root = 1;
ALTER TABLE local_sessions DROP COLUMN promoted_to_root;

-- +goose Down
-- Lossy on purpose, and it cannot be otherwise: Up discards which roots were
-- flag-promoted and what their former parents were. Down restores the column's
-- shape (every row unpromoted) so an older binary can read the table; it does
-- not restore the old family edges. Recover those from the pre-migration
-- backup taken at startup, not from this statement.
ALTER TABLE local_sessions ADD COLUMN promoted_to_root INTEGER NOT NULL DEFAULT 0 CHECK (promoted_to_root IN (0, 1));
