-- +goose Up
CREATE TABLE centralstore_metadata (
    key TEXT PRIMARY KEY NOT NULL,
    value TEXT NOT NULL
) STRICT;

-- +goose Down
DROP TABLE centralstore_metadata;
