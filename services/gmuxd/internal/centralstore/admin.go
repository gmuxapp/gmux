package centralstore

// Admin surface for `gmux daemon state check|backup|export` (cutover design
// §5). All schema-aware SQL for the admin tooling lives here so
// internal/statetool (transport, offline gating, output shaping) never
// learns table names. Everything in this file is read-only against the
// domain tables; BackupInto writes only to its target file.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore/internal/db"
)

// CheckFinding is one violated invariant reported by CheckState. Code is a
// stable machine identifier; Message is human-readable and never contains
// secrets.
type CheckFinding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrBackupTargetExists marks a BackupInto call against an existing target
// file. Backups never overwrite (design §5).
var ErrBackupTargetExists = errors.New("centralstore: backup target already exists")

// OpenReadOnly opens an existing database read-only, without migrating and
// without taking any write lock. It is the admin tooling's offline handle:
// the caller (statetool's offline gate) is responsible for proving no
// daemon owns the database before trusting reads for diagnostics.
// A missing database file returns ErrDatabaseMissing.
func OpenReadOnly(ctx context.Context, dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("centralstore: empty state directory")
	}
	path := DatabasePath(dir)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrDatabaseMissing, path)
	} else if err != nil {
		return nil, fmt.Errorf("centralstore: stat database: %w", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("centralstore: absolute database path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String() +
		"?mode=ro&_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("centralstore: open database read-only: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("centralstore: connect read-only: %w", err)
	}
	// OpenReadOnly is a single-purpose offline handle; share the same pool
	// for both read and write queries (the entire handle is read-only).
	return &Store{database: database, readDB: database, queries: db.New(database), readQ: db.New(database)}, nil
}

// EmbeddedSchemaVersion returns the highest migration version compiled into
// this binary — the version CheckState expects the database to carry.
func EmbeddedSchemaVersion() (int64, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return 0, fmt.Errorf("centralstore: read embedded migrations: %w", err)
	}
	var head int64
	for _, e := range entries {
		name := e.Name()
		idx := strings.IndexByte(name, '_')
		if idx <= 0 {
			continue
		}
		v, convErr := strconv.ParseInt(name[:idx], 10, 64)
		if convErr != nil {
			continue
		}
		if v > head {
			head = v
		}
	}
	if head == 0 {
		return 0, errors.New("centralstore: no embedded migrations found")
	}
	return head, nil
}

// CheckState runs the design §5 `state check`: SQLite integrity
// (`PRAGMA integrity_check`), foreign-key consistency
// (`PRAGMA foreign_key_check`), schema version against the embedded head,
// and the domain invariants (dense sibling positions per scope, stored
// sibling_scope == desiredScope recomputation, dismissal/exit contracts,
// placement orphans, launch-parent acyclicity, adapter non-emptiness,
// manual-peer name uniqueness, catalog order density, owned-entry-only
// rules/placements).
//
// The returned findings are the violations; a nil/empty slice means the
// database is healthy. The error return is operational (the check itself
// could not run). Works on read-write and read-only handles alike.
func (s *Store) CheckState(ctx context.Context) ([]CheckFinding, error) {
	var findings []CheckFinding
	add := func(code, format string, args ...any) {
		findings = append(findings, CheckFinding{Code: code, Message: fmt.Sprintf(format, args...)})
	}

	// 1. Page-level integrity (full check, not quick_check: user-initiated
	// diagnostics can afford the thorough pass).
	integrity, err := stringColumn(ctx, s.database, "PRAGMA integrity_check")
	if err != nil {
		return nil, fmt.Errorf("centralstore: integrity_check: %w", err)
	}
	if len(integrity) != 1 || integrity[0] != "ok" {
		for _, line := range integrity {
			add("integrity", "integrity_check: %s", line)
		}
	}

	// 2. Foreign-key consistency (placement orphans among others; FKs are
	// enforced at runtime but a corrupted or externally-edited database can
	// still violate them).
	fkRows, err := s.database.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, fmt.Errorf("centralstore: foreign_key_check: %w", err)
	}
	defer fkRows.Close()
	for fkRows.Next() {
		var table, parent string
		var rowid, fkid sql.NullInt64
		if err := fkRows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, fmt.Errorf("centralstore: foreign_key_check scan: %w", err)
		}
		add("foreign_key", "row %d of %q references a missing %q row", rowid.Int64, table, parent)
	}
	if err := fkRows.Err(); err != nil {
		return nil, fmt.Errorf("centralstore: foreign_key_check: %w", err)
	}

	// 3. Schema version vs embedded head.
	head, err := EmbeddedSchemaVersion()
	if err != nil {
		return nil, err
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("centralstore: schema version read: %w", err)
	}
	if version != head {
		add("schema_version", "database schema version %d does not match embedded head %d", version, head)
	}

	// 4. Domain invariants, all read from one transaction so the report is
	// a consistent point-in-time view.
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	domain, err := domainInvariantFindings(ctx, tx, s.queries.WithTx(tx))
	if err != nil {
		return nil, err
	}
	findings = append(findings, domain...)
	return findings, nil
}

func domainInvariantFindings(ctx context.Context, tx *sql.Tx, q *db.Queries) ([]CheckFinding, error) {
	var findings []CheckFinding
	add := func(code, format string, args ...any) {
		findings = append(findings, CheckFinding{Code: code, Message: fmt.Sprintf(format, args...)})
	}

	// Dense 0..n-1 positions per (project, sibling scope).
	sparse, err := stringColumn(ctx, tx, `
		SELECT project_entry_id || ' ' || sibling_scope
		FROM project_placements
		GROUP BY project_entry_id, sibling_scope
		HAVING MIN(position) <> 0
		    OR MAX(position) <> COUNT(*) - 1
		    OR COUNT(DISTINCT position) <> COUNT(*)
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("centralstore: scope density query: %w", err)
	}
	for _, s := range sparse {
		add("scope_not_dense", "sibling scope (project %s) positions are not dense 0..n-1", s)
	}

	// Stored sibling_scope == desiredScope recomputation (S2 review fable
	// L-3: the wire decompose groups by stored scope while ReorderSiblings
	// recomputes membership; divergence fails safe but must be surfaced).
	all, err := placements(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("centralstore: placement read: %w", err)
	}
	for _, r := range all {
		if want := desiredScope(all, r); r.scope != want {
			add("scope_mismatch", "placement %s stores sibling scope %q but recomputes to %q", recKey(r), r.scope, want)
		}
	}

	// Dismissal contract: dismissed rows are hidden-not-forgotten and hold
	// no placement (DismissSessionTree removes it in the same transaction).
	dismissedPlaced, err := stringColumn(ctx, tx, `
		SELECT s.id FROM local_sessions s
		JOIN project_placements p ON p.local_session_id = s.id
		WHERE s.dismissed_at_ms IS NOT NULL
		ORDER BY s.id`)
	if err != nil {
		return nil, fmt.Errorf("centralstore: dismissed placement query: %w", err)
	}
	for _, id := range dismissedPlaced {
		add("dismissed_placement", "dismissed session %q still holds a project placement", id)
	}

	// Exit contract: an exit code is only meaningful with a durable exit
	// stamp (the sweep synthesizes exits with a NULL code, never the
	// inverse).
	exitless, err := stringColumn(ctx, tx, `
		SELECT id FROM local_sessions
		WHERE exit_code IS NOT NULL AND exited_at_ms IS NULL
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("centralstore: exit contract query: %w", err)
	}
	for _, id := range exitless {
		add("exit_contract", "session %q carries an exit code without an exit timestamp", id)
	}

	// Launch-parent acyclicity. UNION (not UNION ALL) deduplicates visited
	// pairs so the recursion terminates even on a cyclic graph. This
	// materializes descendant/ancestor pairs and is pathologically O(n²)
	// for a deep chain; accepted for a user-invoked diagnostic over
	// sidebar-scale state (do not put it on a hot path).
	cyclic, err := stringColumn(ctx, tx, `
		WITH RECURSIVE anc(start, cur) AS (
			SELECT id, launch_parent_id FROM local_sessions
			WHERE launch_parent_id IS NOT NULL
			UNION
			SELECT a.start, s.launch_parent_id
			FROM anc a JOIN local_sessions s ON s.id = a.cur
			WHERE s.launch_parent_id IS NOT NULL
		)
		SELECT DISTINCT start FROM anc WHERE cur = start ORDER BY start`)
	if err != nil {
		return nil, fmt.Errorf("centralstore: launch cycle query: %w", err)
	}
	for _, id := range cyclic {
		add("launch_cycle", "session %q participates in a launch-parent cycle", id)
	}

	// Adapter non-emptiness (CHECK-enforced at runtime; still verified for
	// externally corrupted databases).
	emptyAdapter, err := stringColumn(ctx, tx, `
		SELECT id FROM local_sessions WHERE length(adapter) = 0 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("centralstore: adapter query: %w", err)
	}
	for _, id := range emptyAdapter {
		add("adapter_empty", "session %q has an empty adapter", id)
	}

	// Manual-peer name uniqueness (UNIQUE-enforced at runtime).
	dupPeers, err := stringColumn(ctx, tx, `
		SELECT name FROM manual_peers GROUP BY name HAVING COUNT(*) > 1 ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("centralstore: peer uniqueness query: %w", err)
	}
	for _, name := range dupPeers {
		add("peer_name_duplicate", "manual peer name %q appears more than once", name)
	}

	// Catalog invariants: dense sidebar order, dense per-entry rule order.
	// Duplicate sidebar_order values are separately impossible through SQL
	// because the column is UNIQUE; like peer-name uniqueness, that is a
	// UNIQUE-backed residual rather than something this density expression
	// needs to duplicate.
	catalogGaps, err := stringColumn(ctx, tx, `
		SELECT 'entry ' || id FROM project_entries
		WHERE sidebar_order <> (
			SELECT COUNT(*) FROM project_entries other
			WHERE other.sidebar_order < project_entries.sidebar_order
		)
		ORDER BY sidebar_order`)
	if err != nil {
		return nil, fmt.Errorf("centralstore: catalog order query: %w", err)
	}
	for _, e := range catalogGaps {
		add("catalog_order", "project %s breaks the dense sidebar order", e)
	}
	ruleGaps, err := stringColumn(ctx, tx, `
		SELECT project_entry_id
		FROM project_match_rules
		GROUP BY project_entry_id
		HAVING MIN(rule_order) <> 0
		    OR MAX(rule_order) <> COUNT(*) - 1
		    OR COUNT(DISTINCT rule_order) <> COUNT(*)
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("centralstore: rule order query: %w", err)
	}
	for _, e := range ruleGaps {
		add("catalog_order", "project entry %s has a non-dense match rule order", e)
	}

	// Rules and placements are legal only on owned entries (trigger-enforced
	// at runtime).
	nonOwnedRules, err := stringColumn(ctx, tx, `
		SELECT DISTINCT r.project_entry_id FROM project_match_rules r
		JOIN project_entries e ON e.id = r.project_entry_id
		WHERE e.entry_kind <> 'owned' ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("centralstore: non-owned rules query: %w", err)
	}
	for _, e := range nonOwnedRules {
		add("non_owned_rules", "reference entry %s carries match rules", e)
	}
	nonOwnedPlacements, err := stringColumn(ctx, tx, `
		SELECT DISTINCT p.project_entry_id FROM project_placements p
		JOIN project_entries e ON e.id = p.project_entry_id
		WHERE e.entry_kind <> 'owned' ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("centralstore: non-owned placements query: %w", err)
	}
	for _, e := range nonOwnedPlacements {
		add("non_owned_placement", "reference entry %s carries placements", e)
	}
	return findings, nil
}

type queryContexter interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func stringColumn(ctx context.Context, db queryContexter, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// BackupInto writes a transaction-consistent, compacted copy of the
// database to target via `VACUUM INTO` (never a raw file copy of a live
// main file). The target must not exist. The copy is created inside a
// private 0700 temporary directory, tightened to 0600, and hard-linked
// into place (link fails if the target appeared meanwhile — no overwrite
// window). Missing parent directories are created owner-only.
//
// Known cost (design §5 / fable L-6): on the daemon's single connection a
// backup serializes against all domain work for its duration. Backups are
// strictly user-initiated and the database is sidebar-scale.
//
// The backup CONTAINS PEER TOKENS (secrets); callers must tell the user.
func (s *Store) BackupInto(ctx context.Context, target string) error {
	if target == "" {
		return errors.New("centralstore: empty backup target")
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("centralstore: absolute backup target: %w", err)
	}
	if _, err := os.Stat(absolute); err == nil {
		return fmt.Errorf("%w: %s", ErrBackupTargetExists, absolute)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("centralstore: stat backup target: %w", err)
	}
	dir := filepath.Dir(absolute)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("centralstore: create backup directory: %w", err)
	}
	// Same-directory temp dir: MkdirTemp creates it 0700, and the final
	// hard link never crosses filesystems.
	tmpDir, err := os.MkdirTemp(dir, ".gmux-backup-*")
	if err != nil {
		return fmt.Errorf("centralstore: create backup staging directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	staged := filepath.Join(tmpDir, "backup.db")
	if _, err := s.database.ExecContext(ctx, "VACUUM INTO ?", staged); err != nil {
		return fmt.Errorf("centralstore: vacuum into backup: %w", err)
	}
	if err := os.Chmod(staged, 0o600); err != nil {
		return fmt.Errorf("centralstore: secure backup: %w", err)
	}
	if s.beforeBackupLink != nil {
		s.beforeBackupLink()
	}
	if err := os.Link(staged, absolute); err != nil {
		// The atomic no-overwrite guard may lose a race after the early
		// Stat. Preserve the same typed outcome so the HTTP route returns
		// 409 rather than degrading this safe refusal to 500.
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrBackupTargetExists, absolute)
		}
		return fmt.Errorf("centralstore: finalize backup: %w", err)
	}
	return nil
}

// PlacementExport is one durable placement row for state export, joined to
// its project slug. Exactly one subject arm is populated (local session vs
// Local-peer pair), mirroring the schema.
type PlacementExport struct {
	ProjectSlug         string
	LocalSessionID      string
	LocalPeerKey        string
	PeerSessionID       string
	PeerParentSessionID string
	SiblingScope        string
	Position            int64
}

// StateExport is the raw, deterministic-ordered data for
// `gmux daemon state export`. Peers are pre-redacted (RedactedManualPeer);
// URL userinfo scrubbing and JSON shaping are statetool's job.
type StateExport struct {
	SchemaVersion int64
	Sessions      []Session // ALL rows, dismissed included, sorted by ID
	Catalog       ProjectCatalog
	Placements    []PlacementExport
	Peers         []RedactedManualPeer // sorted by name
}

// ExportState reads the export inventory. Sessions, catalog, placements,
// and peers come from one read transaction; the schema version is read
// after (migration bookkeeping never changes under a serving daemon).
func (s *Store) ExportState(ctx context.Context) (StateExport, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return StateExport{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)

	var out StateExport
	rows, err := q.ListSessions(ctx)
	if err != nil {
		return StateExport{}, err
	}
	out.Sessions = make([]Session, 0, len(rows))
	for _, r := range rows {
		v, convErr := sessionFromDB(r)
		if convErr != nil {
			return StateExport{}, convErr
		}
		out.Sessions = append(out.Sessions, v)
	}
	sort.Slice(out.Sessions, func(i, j int) bool { return out.Sessions[i].ID < out.Sessions[j].ID })

	if out.Catalog, err = catalogFromQueries(ctx, q); err != nil {
		return StateExport{}, err
	}
	slugByEntry := make(map[int64]string, len(out.Catalog))
	for _, e := range out.Catalog {
		slugByEntry[int64(e.ID)] = e.Slug
	}

	placementRows, err := placements(ctx, q)
	if err != nil {
		return StateExport{}, err
	}
	out.Placements = make([]PlacementExport, 0, len(placementRows))
	for _, p := range placementRows {
		slug, ok := slugByEntry[p.project]
		if !ok {
			return StateExport{}, fmt.Errorf("centralstore: placement references unknown project entry %d", p.project)
		}
		e := PlacementExport{ProjectSlug: slug, SiblingScope: p.scope, Position: p.pos}
		if p.local != "" {
			e.LocalSessionID = p.local
		} else {
			e.LocalPeerKey, e.PeerSessionID, e.PeerParentSessionID = p.peer, p.session, p.parent
		}
		out.Placements = append(out.Placements, e)
	}
	sort.Slice(out.Placements, func(i, j int) bool {
		a, b := out.Placements[i], out.Placements[j]
		if a.ProjectSlug != b.ProjectSlug {
			return a.ProjectSlug < b.ProjectSlug
		}
		if a.SiblingScope != b.SiblingScope {
			return a.SiblingScope < b.SiblingScope
		}
		return a.Position < b.Position
	})

	peerRows, err := q.ListManualPeers(ctx)
	if err != nil {
		return StateExport{}, err
	}
	out.Peers = make([]RedactedManualPeer, 0, len(peerRows))
	for _, r := range peerRows {
		v, convErr := manualPeerFromDB(r)
		if convErr != nil {
			return StateExport{}, convErr
		}
		out.Peers = append(out.Peers, v.Redacted())
	}
	sort.Slice(out.Peers, func(i, j int) bool { return out.Peers[i].Name < out.Peers[j].Name })

	if err := tx.Commit(); err != nil {
		return StateExport{}, err
	}
	out.SchemaVersion, err = s.SchemaVersion(ctx)
	if err != nil {
		return StateExport{}, fmt.Errorf("centralstore: schema version read: %w", err)
	}
	return out, nil
}
