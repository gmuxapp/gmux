-- +goose Up
-- Reference node_id (review fable H-1, decided): a project reference's
-- NodeID is viewer-owned durable state per ADR 0017 — the stable opaque
-- identity anchor that keeps a reference matching the right host across
-- renames and stops a reused name from matching the wrong one. Dropping it
-- at the cutover would silently degrade every reference to name-only
-- matching. NULL for owned entries and for references created against
-- pre-ADR-0007 daemons (name-only fallback, same as production).
--
-- This is a new migration rather than an amendment of 00002: 00002 landed
-- in an earlier, already-integrated slice; rewriting it would rewrite
-- reviewed history and strand any existing dev database whose goose
-- version already records 2+.
ALTER TABLE project_entries ADD COLUMN node_id TEXT
    CHECK (node_id IS NULL OR length(node_id) > 0);

-- Status-reported fact (review fable M-1 / FD-7, decided NOT an accepted
-- diff): production distinguishes "the runner never reported a status"
-- (Status == nil, wire "status": null) from a reported all-false status,
-- and `gmux wait`'s terminalReason keys its died/idle verdict on exactly
-- that distinction (ADR 0023 turn-state-at-death parity). A bit is the
-- cleaner shape versus a nullable working column: working/has_error stay
-- NOT NULL with their CHECKs. Runner-authoritative and GENERATION-SCOPED
-- (delta review Δ-1): set on the first observation that carries a
-- working/error fact, never set by daemon-side acknowledgement, sticky
-- within a generation, and reset by a replacement generation alongside
-- working/error/started_at — production re-registration replaces Status
-- wholesale from the new runner's /meta, which is nil until the new
-- process reports (discovery.go:290).
ALTER TABLE local_sessions ADD COLUMN status_reported INTEGER NOT NULL
    DEFAULT 0 CHECK (status_reported IN (0, 1));

-- +goose Down
ALTER TABLE local_sessions DROP COLUMN status_reported;
ALTER TABLE project_entries DROP COLUMN node_id;
