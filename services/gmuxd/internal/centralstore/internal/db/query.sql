-- name: PutMetadata :exec
INSERT INTO centralstore_metadata (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;

-- name: GetMetadata :one
SELECT value FROM centralstore_metadata WHERE key = ?;

-- name: InsertSession :one
INSERT INTO local_sessions (
    id, adapter, conversation_ref, command_json, cwd, workspace_root,
    remotes_json, slug, shell_title, adapter_title, subtitle,
    working, unread, has_error, created_at_ms, started_at_ms, exited_at_ms,
    last_activity_at_ms, exit_code, terminal_cols, terminal_rows, launch_parent_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetSession :one
SELECT * FROM local_sessions WHERE id = ?;

-- name: ListSessions :many
SELECT * FROM local_sessions ORDER BY id;

-- name: SessionVersion :one
SELECT row_version FROM local_sessions WHERE id = ?;

-- name: AcknowledgeSessionAtVersion :execrows
UPDATE local_sessions
SET unread = 0, has_error = 0, row_version = row_version + 1
WHERE id = ? AND row_version = ? AND (unread <> 0 OR has_error <> 0);

-- name: UpdateCommonFacts :execrows
UPDATE local_sessions SET
    row_version = row_version + 1,
    adapter = ?, conversation_ref = ?, command_json = ?, cwd = ?,
    workspace_root = ?, remotes_json = ?, slug = ?, shell_title = ?,
    adapter_title = ?, subtitle = ?, working = ?, unread = ?, has_error = ?,
    started_at_ms = ?, exited_at_ms = ?, last_activity_at_ms = ?, exit_code = ?,
    terminal_cols = ?, terminal_rows = ?
WHERE id = ? AND row_version = ?;

-- name: ClearDirectChildParents :execrows
UPDATE local_sessions
SET launch_parent_id = NULL, row_version = row_version + 1
WHERE launch_parent_id = ?;

-- name: DeleteSessionAtVersion :execrows
DELETE FROM local_sessions WHERE id = ? AND row_version = ?;

-- name: SetPromotion :execrows
UPDATE local_sessions
SET promoted_to_root = ?, row_version = row_version + 1
WHERE id = ? AND promoted_to_root <> ?;

-- name: ListProjectEntries :many
SELECT * FROM project_entries ORDER BY sidebar_order;

-- name: ListProjectRules :many
SELECT * FROM project_match_rules ORDER BY project_entry_id, rule_order;

-- name: ParkProjectEntries :exec
UPDATE project_entries SET sidebar_order = sidebar_order + ?;

-- name: InsertProjectEntry :one
INSERT INTO project_entries
(sidebar_order, entry_kind, slug, peer_key, created_at_ms, updated_at_ms)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: UpdateProjectEntry :execrows
UPDATE project_entries
SET sidebar_order = ?, slug = ?, updated_at_ms = ?
WHERE id = ?;

-- name: ParkProjectEntrySlug :execrows
UPDATE project_entries SET slug = ? WHERE id = ?;

-- name: FinalizeProjectEntryOrder :execrows
UPDATE project_entries SET sidebar_order = ? WHERE id = ?;

-- name: DeleteProjectEntry :execrows
DELETE FROM project_entries WHERE id = ?;

-- name: DeleteProjectRules :exec
DELETE FROM project_match_rules WHERE project_entry_id = ?;

-- name: InsertProjectRule :exec
INSERT INTO project_match_rules
(project_entry_id, rule_order, path, remote, exact)
VALUES (?, ?, ?, ?, ?);

-- name: OwnedProjectExists :one
SELECT EXISTS(
    SELECT 1 FROM project_entries WHERE id = ? AND entry_kind = 'owned'
);

-- name: PlacementCount :one
SELECT COUNT(*) FROM project_placements;

-- name: ListPlacements :many
SELECT p.id, p.project_entry_id, p.local_session_id, p.local_peer_key,
       p.peer_session_id, p.peer_parent_session_id, p.sibling_scope, p.position,
       COALESCE(s.created_at_ms, 0) AS local_created_at_ms,
       COALESCE(s.promoted_to_root, 0) AS local_promoted_to_root,
       s.launch_parent_id
FROM project_placements p
LEFT JOIN local_sessions s ON s.id = p.local_session_id
ORDER BY p.project_entry_id, p.sibling_scope, p.position, p.id;

-- name: LocalSessionPlacementFacts :one
SELECT created_at_ms, promoted_to_root, launch_parent_id
FROM local_sessions WHERE id = ?;

-- name: ParkPlacement :execrows
UPDATE project_placements SET sibling_scope = ?, position = 0 WHERE id = ?;

-- name: FinalizeLocalPlacement :execrows
UPDATE project_placements
SET project_entry_id = ?, sibling_scope = ?, position = ?
WHERE id = ? AND local_session_id IS NOT NULL;

-- name: FinalizeLocalPeerPlacement :execrows
UPDATE project_placements
SET project_entry_id = ?, peer_parent_session_id = ?, sibling_scope = ?, position = ?
WHERE id = ? AND local_session_id IS NULL;

-- name: InsertLocalPlacement :one
INSERT INTO project_placements
(project_entry_id, local_session_id, sibling_scope, position)
VALUES (?, ?, ?, ?)
RETURNING id;

-- name: InsertLocalPeerPlacement :one
INSERT INTO project_placements
(project_entry_id, local_peer_key, peer_session_id, peer_parent_session_id,
 sibling_scope, position)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: DeleteLocalPeerPlacements :execrows
DELETE FROM project_placements WHERE local_peer_key = ?;

-- name: TemporaryPlacementCount :one
SELECT COUNT(*) FROM project_placements WHERE sibling_scope LIKE '~:%';

