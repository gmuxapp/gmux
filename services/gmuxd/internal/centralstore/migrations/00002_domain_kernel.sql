-- +goose Up

CREATE TABLE local_sessions (
    id                    TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    row_version           INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),

    adapter               TEXT NOT NULL CHECK (length(adapter) > 0),
    conversation_ref      TEXT,
    command_json          TEXT NOT NULL CHECK (
                              json_valid(command_json) AND
                              json_type(command_json) = 'array'
                          ),
    cwd                   TEXT NOT NULL,
    workspace_root        TEXT,
    remotes_json          TEXT NOT NULL CHECK (
                              json_valid(remotes_json) AND
                              json_type(remotes_json) = 'object'
                          ),

    slug                  TEXT,
    -- Runner-owned naming proposal; slug is the allocated URL key.
    slug_base             TEXT,
    shell_title           TEXT,
    adapter_title         TEXT,
    subtitle              TEXT,
    working               INTEGER NOT NULL DEFAULT 0 CHECK (working IN (0, 1)),
    unread                INTEGER NOT NULL DEFAULT 0 CHECK (unread IN (0, 1)),
    has_error             INTEGER NOT NULL DEFAULT 0 CHECK (has_error IN (0, 1)),

    created_at_ms         INTEGER NOT NULL CHECK (created_at_ms >= 0),
    started_at_ms         INTEGER CHECK (started_at_ms IS NULL OR started_at_ms >= 0),
    exited_at_ms          INTEGER CHECK (exited_at_ms IS NULL OR exited_at_ms >= 0),
    last_activity_at_ms   INTEGER CHECK (last_activity_at_ms IS NULL OR last_activity_at_ms >= 0),
    dismissed_at_ms       INTEGER CHECK (dismissed_at_ms IS NULL OR dismissed_at_ms >= 0),
    exit_code             INTEGER,
    terminal_cols         INTEGER CHECK (terminal_cols IS NULL OR terminal_cols BETWEEN 1 AND 65535),
    terminal_rows         INTEGER CHECK (terminal_rows IS NULL OR terminal_rows BETWEEN 1 AND 65535),

    -- Intentional non-FK: a child can register before its parent.
    launch_parent_id      TEXT,
    promoted_to_root      INTEGER NOT NULL DEFAULT 0 CHECK (promoted_to_root IN (0, 1)),

    CHECK (launch_parent_id IS NULL OR
           (length(launch_parent_id) > 0 AND launch_parent_id <> id)),
    CHECK ((terminal_cols IS NULL) = (terminal_rows IS NULL))
) STRICT;

CREATE UNIQUE INDEX local_sessions_adapter_slug_unique_idx
    ON local_sessions(adapter, slug)
    WHERE slug IS NOT NULL AND slug <> '';
CREATE INDEX local_sessions_adapter_candidate_idx
    ON local_sessions(adapter, dismissed_at_ms, row_version, id);
CREATE INDEX local_sessions_parent_idx
    ON local_sessions(launch_parent_id)
    WHERE launch_parent_id IS NOT NULL;
CREATE INDEX local_sessions_conversation_idx
    ON local_sessions(adapter, conversation_ref)
    WHERE conversation_ref IS NOT NULL;
CREATE INDEX local_sessions_visible_activity_idx
    ON local_sessions(last_activity_at_ms DESC, id)
    WHERE dismissed_at_ms IS NULL;

CREATE TABLE project_entries (
    id                    INTEGER PRIMARY KEY,
    sidebar_order         INTEGER NOT NULL UNIQUE CHECK (sidebar_order >= 0),
    entry_kind            TEXT NOT NULL CHECK (entry_kind IN ('owned', 'reference')),
    slug                  TEXT NOT NULL CHECK (length(slug) > 0),

    -- Neutral current peer key/name semantics. No stable NodeID or peer FK
    -- is assumed before the later peer slice.
    peer_key              TEXT,

    created_at_ms         INTEGER NOT NULL CHECK (created_at_ms >= 0),
    updated_at_ms         INTEGER NOT NULL CHECK (updated_at_ms >= 0),

    CHECK (
        (entry_kind = 'owned' AND peer_key IS NULL)
        OR
        (entry_kind = 'reference' AND peer_key IS NOT NULL AND length(peer_key) > 0)
    )
) STRICT;

CREATE UNIQUE INDEX project_entries_owned_slug_unique_idx
    ON project_entries(slug) WHERE entry_kind = 'owned';
CREATE UNIQUE INDEX project_entries_reference_unique_idx
    ON project_entries(peer_key, slug) WHERE entry_kind = 'reference';
CREATE INDEX project_entries_order_idx
    ON project_entries(sidebar_order);

CREATE TABLE project_match_rules (
    id                    INTEGER PRIMARY KEY,
    project_entry_id      INTEGER NOT NULL
                              REFERENCES project_entries(id) ON DELETE CASCADE,
    rule_order            INTEGER NOT NULL CHECK (rule_order >= 0),
    path                  TEXT,
    remote                TEXT,
    exact                 INTEGER NOT NULL DEFAULT 0 CHECK (exact IN (0, 1)),
    CHECK ((path IS NOT NULL) <> (remote IS NOT NULL)),
    CHECK (path IS NULL OR length(path) > 0),
    CHECK (remote IS NULL OR length(remote) > 0),
    CHECK (exact = 0 OR path IS NOT NULL),
    UNIQUE (project_entry_id, rule_order)
) STRICT;

CREATE UNIQUE INDEX project_match_rules_path_unique_idx
    ON project_match_rules(path) WHERE path IS NOT NULL;
CREATE INDEX project_match_rules_project_idx
    ON project_match_rules(project_entry_id, rule_order);
CREATE INDEX project_match_rules_remote_idx
    ON project_match_rules(remote) WHERE remote IS NOT NULL;

CREATE TABLE project_placements (
    id                    INTEGER PRIMARY KEY,
    project_entry_id      INTEGER NOT NULL
                              REFERENCES project_entries(id) ON DELETE CASCADE,

    -- Exactly one subject arm is populated.
    local_session_id      TEXT UNIQUE
                              REFERENCES local_sessions(id) ON DELETE CASCADE,
    local_peer_key        TEXT,
    peer_session_id       TEXT,
    peer_parent_session_id TEXT,

    sibling_scope         TEXT NOT NULL CHECK (length(sibling_scope) > 0),
    position              INTEGER NOT NULL CHECK (position >= 0),

    CHECK (
        (local_session_id IS NOT NULL AND
         local_peer_key IS NULL AND peer_session_id IS NULL AND
         peer_parent_session_id IS NULL)
        OR
        (local_session_id IS NULL AND
         local_peer_key IS NOT NULL AND length(local_peer_key) > 0 AND
         peer_session_id IS NOT NULL AND length(peer_session_id) > 0)
    ),
    CHECK (peer_parent_session_id IS NULL OR
           (length(peer_parent_session_id) > 0 AND
            peer_parent_session_id <> peer_session_id)),
    UNIQUE (local_peer_key, peer_session_id),
    UNIQUE (project_entry_id, sibling_scope, position)
) STRICT;

CREATE INDEX project_placements_project_scope_idx
    ON project_placements(project_entry_id, sibling_scope, position);
CREATE INDEX project_placements_local_peer_idx
    ON project_placements(local_peer_key)
    WHERE local_session_id IS NULL;

-- Rules and placements are legal only for owned entries. FKs alone cannot
-- express a predicate on the parent row.
-- +goose StatementBegin
CREATE TRIGGER project_match_rules_owned_insert
BEFORE INSERT ON project_match_rules
WHEN NOT EXISTS (
    SELECT 1 FROM project_entries
    WHERE id = NEW.project_entry_id AND entry_kind = 'owned'
)
BEGIN
    SELECT RAISE(ABORT, 'match rules require an owned project entry');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER project_match_rules_owned_update
BEFORE UPDATE OF project_entry_id ON project_match_rules
WHEN NOT EXISTS (
    SELECT 1 FROM project_entries
    WHERE id = NEW.project_entry_id AND entry_kind = 'owned'
)
BEGIN
    SELECT RAISE(ABORT, 'match rules require an owned project entry');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER project_placements_owned_insert
BEFORE INSERT ON project_placements
WHEN NOT EXISTS (
    SELECT 1 FROM project_entries
    WHERE id = NEW.project_entry_id AND entry_kind = 'owned'
)
BEGIN
    SELECT RAISE(ABORT, 'placements require an owned project entry');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER project_placements_owned_update
BEFORE UPDATE OF project_entry_id ON project_placements
WHEN NOT EXISTS (
    SELECT 1 FROM project_entries
    WHERE id = NEW.project_entry_id AND entry_kind = 'owned'
)
BEGIN
    SELECT RAISE(ABORT, 'placements require an owned project entry');
END;
-- +goose StatementEnd

-- An entry cannot change kind. Replacing owned/reference entries is a domain
-- delete+insert/reorder operation, not an in-place shape mutation.
-- +goose StatementBegin
CREATE TRIGGER project_entries_kind_immutable
BEFORE UPDATE OF entry_kind, peer_key ON project_entries
WHEN OLD.entry_kind IS NOT NEW.entry_kind OR OLD.peer_key IS NOT NEW.peer_key
BEGIN
    SELECT RAISE(ABORT, 'project entry kind and peer key are immutable');
END;
-- +goose StatementEnd

-- Detect a cycle both in ordinary parent-first insertion and when NEW.id was
-- previously referenced as an absent parent by an already stored child.
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

-- The launch relationship is immutable once set (ADR 0026 §8): a parent may
-- only ever be cleared to NULL (parent deletion), never rewritten, so the
-- recursive insert trigger above stays sufficient for cycle prevention.
-- +goose StatementBegin
CREATE TRIGGER local_sessions_launch_parent_immutable_update
BEFORE UPDATE OF launch_parent_id ON local_sessions
WHEN NEW.launch_parent_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'launch parent can only be cleared');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER local_sessions_launch_parent_immutable_update;
DROP TRIGGER local_sessions_launch_parent_no_cycle_insert;
DROP TRIGGER project_entries_kind_immutable;
DROP TRIGGER project_placements_owned_update;
DROP TRIGGER project_placements_owned_insert;
DROP TRIGGER project_match_rules_owned_update;
DROP TRIGGER project_match_rules_owned_insert;
DROP TABLE project_placements;
DROP TABLE project_match_rules;
DROP TABLE project_entries;
DROP TABLE local_sessions;
