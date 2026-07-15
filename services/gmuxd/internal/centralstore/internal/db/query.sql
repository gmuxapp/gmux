-- name: PutMetadata :exec
INSERT INTO centralstore_metadata (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;

-- name: GetMetadata :one
SELECT value FROM centralstore_metadata WHERE key = ?;
