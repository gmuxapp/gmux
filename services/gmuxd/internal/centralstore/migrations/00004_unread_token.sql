-- +goose Up
ALTER TABLE local_sessions ADD COLUMN unread_token TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE local_sessions DROP COLUMN unread_token;
