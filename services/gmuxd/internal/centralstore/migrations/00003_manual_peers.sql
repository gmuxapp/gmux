-- +goose Up

-- Manual peers are the only durable peer kind (ADR 0026 §1/§3/§9):
-- runtime-discovered peers (tailscale, devcontainers) and all connection
-- state stay in the peering manager. Rows map 1:1 to the fields production
-- persists in peers.json (internal/peerstore Record): display/routing name,
-- base URL, optional bearer token, optional opaque node identity (ADR 0007).
--
-- token is a SECRET. Raw database backups therefore are secrets; export and
-- diagnostics must go through the redaction seam (ManualPeer.Redacted) and
-- domain code must never place a token in an error or log line.
--
-- Bootstrap identity (the auth-token file) intentionally stays a dedicated
-- file per ADR 0026 §3 and is NOT represented here.
CREATE TABLE manual_peers (
    id            INTEGER PRIMARY KEY,
    row_version   INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),

    name          TEXT NOT NULL UNIQUE CHECK (length(name) > 0),
    url           TEXT NOT NULL CHECK (length(url) > 0),
    token         TEXT CHECK (token IS NULL OR length(token) > 0),
    node_id       TEXT CHECK (node_id IS NULL OR length(node_id) > 0),

    created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
    updated_at_ms INTEGER NOT NULL CHECK (updated_at_ms >= 0)
) STRICT;

-- node_id is the durable host identity used for dedup; at most one manual
-- row may claim a given identity.
CREATE UNIQUE INDEX manual_peers_node_id_unique_idx
    ON manual_peers(node_id) WHERE node_id IS NOT NULL;

-- +goose Down

DROP TABLE manual_peers;
