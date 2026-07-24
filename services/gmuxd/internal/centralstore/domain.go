package centralstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore/internal/db"
)

type UnixMillis int64
type RowVersion int64
type SessionID string
type ProjectEntryID int64
type PeerKey string

type Session struct {
	ID                                              SessionID
	Version                                         RowVersion
	Adapter, ConversationRef                        string
	Command                                         []string
	CWD, WorkspaceRoot                              string
	Remotes                                         map[string]string
	Slug, ShellTitle, AdapterTitle, Subtitle, Title string
	Working, Unread, Error                          bool
	// StatusReported records whether the CURRENT runner generation ever
	// reported a working/error status for this row (runner-authoritative,
	// generation-scoped; the production model's Status != nil, which
	// re-registration replaces wholesale from the new runner's /meta).
	// Sticky within a generation; a replacement generation resets it
	// alongside working/error/started_at. The wire emits "status": null
	// while it is false, and gmux wait's terminalReason keys its died/idle
	// verdict on the distinction (ADR 0023).
	StatusReported                                   bool
	CreatedAt                                        UnixMillis
	StartedAt, ExitedAt, LastActivityAt, DismissedAt *UnixMillis
	ExitCode                                         *int
	TerminalCols, TerminalRows                       *uint16
	LaunchParentID                                   *SessionID
	PromotedToRoot                                   bool
}

// NewSession contains only facts legal at registration. New registrations are
// always visible, unpromoted, and start at row version one.
type NewSession struct {
	ID                             SessionID
	Adapter, ConversationRef       string
	Command                        []string
	CWD, WorkspaceRoot             string
	Remotes                        map[string]string
	Slug, ShellTitle, AdapterTitle string
	Subtitle                       string
	Working, Unread, Error         bool
	// StatusReported marks the status facts as runner-reported. Implied
	// (and forced true at insert) when Working or Error is set.
	StatusReported             bool
	CreatedAt                  UnixMillis
	StartedAt, ExitedAt        *UnixMillis
	LastActivityAt             *UnixMillis
	ExitCode                   *int
	TerminalCols, TerminalRows *uint16
	LaunchParentID             *SessionID
}

// NullablePatch has three states: zero means unchanged, Set stores a value,
// and Clear stores SQL NULL. Set and Clear together are invalid.
type NullablePatch[T any] struct {
	Set   *T
	Clear bool
}

type TerminalSize struct{ Cols, Rows uint16 }

type CommonFactsPatch struct {
	Adapter, ConversationRef, CWD, WorkspaceRoot, Slug, ShellTitle, AdapterTitle, Subtitle *string
	Command                                                                                *[]string
	Remotes                                                                                *map[string]string
	Working, Unread, Error                                                                 *bool
	StartedAt, ExitedAt, LastActivityAt                                                    NullablePatch[UnixMillis]
	ExitCode                                                                               NullablePatch[int]
	TerminalSize                                                                           NullablePatch[TerminalSize]
}

type MutationResult struct {
	Changed        bool
	SessionVersion RowVersion
	SessionsDirty  bool
	WorldDirty     bool
}

var (
	ErrStaleVersion = errors.New("centralstore: stale row version")
	// ErrSessionNotFound marks a mutation targeting a session row that does
	// not exist. Session() keeps its (value, ok, err) shape instead.
	ErrSessionNotFound = errors.New("centralstore: session not found")
	// ErrCatalogHasPlacements marks the bootstrap-only catalog boundary. This
	// primitive cannot authoritatively rematch subjects once placement exists.
	ErrCatalogHasPlacements = errors.New("centralstore: catalog replacement requires zero placements")
	ErrLocalPeerParentCycle = errors.New("centralstore: Local-peer parent cycle")
)

type MatchRule struct {
	Path, Remote string
	Exact        bool
}
type OwnedProjectSpec struct {
	Slug  string
	Rules []MatchRule
}
type ProjectReference struct {
	PeerKey PeerKey
	Slug    string
	// NodeID is the referenced peer's stable opaque identity (ADR 0007/
	// 0017): the viewer's liveness anchor, stamped at creation. Empty for
	// references created against pre-ADR-0007 daemons (name-only fallback).
	// Mutable metadata, not identity (identity is PeerKey + Slug).
	NodeID string
}
type ProjectEntrySpec struct {
	ID        ProjectEntryID
	Owned     *OwnedProjectSpec
	Reference *ProjectReference
}
type ProjectEntryKind string

const (
	ProjectEntryOwned     ProjectEntryKind = "owned"
	ProjectEntryReference ProjectEntryKind = "reference"
)

type ProjectEntry struct {
	ID      ProjectEntryID
	Kind    ProjectEntryKind
	Slug    string
	PeerKey PeerKey
	// NodeID is set only on references (ADR 0017 liveness anchor).
	NodeID               string
	Rules                []MatchRule
	CreatedAt, UpdatedAt UnixMillis
}
type ProjectCatalog []ProjectEntry

type LocalPeerSubject struct {
	PeerKey                    PeerKey
	SessionID, ParentSessionID string
}
type SubjectRef struct {
	LocalSessionID SessionID
	LocalPeer      *LocalPeerSubject
}
type ParentRef struct{ Subject *SubjectRef }
type Placement struct {
	ProjectEntryID ProjectEntryID
	Subject        SubjectRef
	Parent         ParentRef
	Position       int
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
func nullString(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }
func nullMillis(v *UnixMillis) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}
func nullInt(v *int) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}
func nullUint(v *uint16) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}
func deriveTitle(s Session) string {
	if s.AdapterTitle != "" {
		return s.AdapterTitle
	}
	if s.ShellTitle != "" {
		return s.ShellTitle
	}
	if len(s.Command) > 0 {
		return s.Command[0]
	}
	return s.Adapter
}

func validateMillis(name string, v *UnixMillis) error {
	if v != nil && *v < 0 {
		return fmt.Errorf("centralstore: %s must be non-negative", name)
	}
	return nil
}
func validateTerminal(cols, rows *uint16) error {
	if (cols == nil) != (rows == nil) {
		return errors.New("centralstore: terminal size requires both columns and rows")
	}
	if cols != nil && (*cols == 0 || *rows == 0) {
		return errors.New("centralstore: terminal dimensions must be positive")
	}
	return nil
}
func validateNewSession(v NewSession) error {
	if v.ID == "" || v.Adapter == "" {
		return errors.New("centralstore: session id and adapter required")
	}
	if v.CreatedAt < 0 {
		return errors.New("centralstore: created timestamp must be non-negative")
	}
	if v.LaunchParentID != nil && (*v.LaunchParentID == "" || *v.LaunchParentID == v.ID) {
		return errors.New("centralstore: invalid launch parent")
	}
	for name, x := range map[string]*UnixMillis{"started timestamp": v.StartedAt, "exited timestamp": v.ExitedAt, "activity timestamp": v.LastActivityAt} {
		if err := validateMillis(name, x); err != nil {
			return err
		}
	}
	return validateTerminal(v.TerminalCols, v.TerminalRows)
}

func marshalWhole(command []string, remotes map[string]string) (string, string, error) {
	if command == nil {
		command = []string{}
	}
	if remotes == nil {
		remotes = map[string]string{}
	}
	c, err := json.Marshal(command)
	if err != nil {
		return "", "", err
	}
	r, err := json.Marshal(remotes)
	if err != nil {
		return "", "", err
	}
	return string(c), string(r), nil
}

func sessionFromDB(v db.LocalSession) (Session, error) {
	out := Session{ID: SessionID(v.ID), Version: RowVersion(v.RowVersion), Adapter: v.Adapter, ConversationRef: v.ConversationRef.String, CWD: v.Cwd, WorkspaceRoot: v.WorkspaceRoot.String, Slug: v.Slug.String, ShellTitle: v.ShellTitle.String, AdapterTitle: v.AdapterTitle.String, Subtitle: v.Subtitle.String, CreatedAt: UnixMillis(v.CreatedAtMs)}
	if v.RowVersion < 1 || v.CreatedAtMs < 0 {
		return Session{}, errors.New("centralstore: corrupt session numeric value")
	}
	for name, x := range map[string]int64{"working": v.Working, "unread": v.Unread, "error": v.HasError, "promotion": v.PromotedToRoot, "status-reported": v.StatusReported} {
		if x != 0 && x != 1 {
			return Session{}, fmt.Errorf("centralstore: corrupt %s boolean", name)
		}
	}
	out.Working = v.Working == 1
	out.Unread = v.Unread == 1
	out.Error = v.HasError == 1
	out.PromotedToRoot = v.PromotedToRoot == 1
	out.StatusReported = v.StatusReported == 1
	if (out.Working || out.Error) && !out.StatusReported {
		return Session{}, errors.New("centralstore: status facts without status-reported bit")
	}
	if err := json.Unmarshal([]byte(v.CommandJson), &out.Command); err != nil {
		return Session{}, fmt.Errorf("centralstore: decode command: %w", err)
	}
	if out.Command == nil {
		return Session{}, errors.New("centralstore: corrupt null command")
	}
	if err := json.Unmarshal([]byte(v.RemotesJson), &out.Remotes); err != nil {
		return Session{}, fmt.Errorf("centralstore: decode remotes: %w", err)
	}
	if out.Remotes == nil {
		return Session{}, errors.New("centralstore: corrupt null remotes")
	}
	ms := func(name string, n sql.NullInt64) (*UnixMillis, error) {
		if !n.Valid {
			return nil, nil
		}
		if n.Int64 < 0 {
			return nil, fmt.Errorf("centralstore: corrupt %s", name)
		}
		x := UnixMillis(n.Int64)
		return &x, nil
	}
	var err error
	if out.StartedAt, err = ms("started timestamp", v.StartedAtMs); err != nil {
		return Session{}, err
	}
	if out.ExitedAt, err = ms("exited timestamp", v.ExitedAtMs); err != nil {
		return Session{}, err
	}
	if out.LastActivityAt, err = ms("activity timestamp", v.LastActivityAtMs); err != nil {
		return Session{}, err
	}
	if out.DismissedAt, err = ms("dismissed timestamp", v.DismissedAtMs); err != nil {
		return Session{}, err
	}
	if v.ExitCode.Valid {
		if int64(int(v.ExitCode.Int64)) != v.ExitCode.Int64 {
			return Session{}, errors.New("centralstore: corrupt exit code")
		}
		x := int(v.ExitCode.Int64)
		out.ExitCode = &x
	}
	toUint := func(n sql.NullInt64) (*uint16, error) {
		if !n.Valid {
			return nil, nil
		}
		if n.Int64 < 1 || n.Int64 > 65535 {
			return nil, errors.New("centralstore: corrupt terminal dimension")
		}
		x := uint16(n.Int64)
		return &x, nil
	}
	if out.TerminalCols, err = toUint(v.TerminalCols); err != nil {
		return Session{}, err
	}
	if out.TerminalRows, err = toUint(v.TerminalRows); err != nil {
		return Session{}, err
	}
	if (out.TerminalCols == nil) != (out.TerminalRows == nil) {
		return Session{}, errors.New("centralstore: corrupt terminal pair")
	}
	if v.LaunchParentID.Valid {
		x := SessionID(v.LaunchParentID.String)
		out.LaunchParentID = &x
	}
	out.Title = deriveTitle(out)
	return out, nil
}

func (s *Store) InsertSession(ctx context.Context, v NewSession) (Session, MutationResult, error) {
	if err := validateNewSession(v); err != nil {
		return Session{}, MutationResult{}, err
	}
	cmd, rem, err := marshalWhole(v.Command, v.Remotes)
	if err != nil {
		return Session{}, MutationResult{}, err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)
	row, err := q.InsertSession(ctx, db.InsertSessionParams{ID: string(v.ID), Adapter: v.Adapter, ConversationRef: nullString(v.ConversationRef), CommandJson: cmd, Cwd: v.CWD, WorkspaceRoot: nullString(v.WorkspaceRoot), RemotesJson: rem, Slug: nullString(v.Slug), ShellTitle: nullString(v.ShellTitle), AdapterTitle: nullString(v.AdapterTitle), Subtitle: nullString(v.Subtitle), Working: boolInt(v.Working), Unread: boolInt(v.Unread), HasError: boolInt(v.Error), StatusReported: boolInt(v.StatusReported || v.Working || v.Error), CreatedAtMs: int64(v.CreatedAt), StartedAtMs: nullMillis(v.StartedAt), ExitedAtMs: nullMillis(v.ExitedAt), LastActivityAtMs: nullMillis(v.LastActivityAt), ExitCode: nullInt(v.ExitCode), TerminalCols: nullUint(v.TerminalCols), TerminalRows: nullUint(v.TerminalRows), LaunchParentID: func() sql.NullString {
		if v.LaunchParentID == nil {
			return sql.NullString{}
		}
		return nullString(string(*v.LaunchParentID))
	}()})
	if err != nil {
		return Session{}, MutationResult{}, fmt.Errorf("centralstore: insert session: %w", err)
	}
	if _, err = normalizePlacements(ctx, q, s.beforePlacementFinalize); err != nil {
		return Session{}, MutationResult{}, err
	}
	out, err := sessionFromDB(row)
	if err != nil {
		return Session{}, MutationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return Session{}, MutationResult{}, err
	}
	return out, MutationResult{Changed: true, SessionVersion: 1, SessionsDirty: true, WorldDirty: true}, nil
}

func (s *Store) Session(ctx context.Context, id SessionID) (Session, bool, error) {
	v, err := s.readQ.GetSession(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	out, err := sessionFromDB(v)
	return out, err == nil, err
}
func (s *Store) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.readQ.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(rows))
	for _, r := range rows {
		v, e := sessionFromDB(r)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}

func applyNullable[T any](dst **T, p NullablePatch[T]) error {
	if p.Set != nil && p.Clear {
		return errors.New("centralstore: nullable patch cannot set and clear")
	}
	if p.Clear {
		*dst = nil
	} else if p.Set != nil {
		x := *p.Set
		*dst = &x
	}
	return nil
}
func (s *Store) ApplyCommonFacts(ctx context.Context, id SessionID, observed RowVersion, p CommonFactsPatch) (MutationResult, error) {
	return s.applyCommonFacts(ctx, id, observed, p, nil)
}

// RunnerObservation is one ordered event from the currently installed runner
// generation. ObservedVersion is the coordinator's durable coordination token.
// ObservedAt is evidence for activity transitions, not an unconditional write.
type RunnerObservation struct {
	ID              SessionID
	ObservedVersion RowVersion
	ObservedAt      UnixMillis
	Facts           RunnerFacts
}

// ApplyRunnerObservation conditionally projects runner-owned facts. The
// coordinator must first prove that the event's generation is current.
func (s *Store) ApplyRunnerObservation(ctx context.Context, o RunnerObservation) (MutationResult, error) {
	if o.ID == "" || o.ObservedAt < 0 {
		return MutationResult{}, errors.New("centralstore: invalid runner observation")
	}
	if err := validateRunnerFacts(o.Facts); err != nil {
		return MutationResult{}, err
	}
	p := CommonFactsPatch{
		ConversationRef: o.Facts.ConversationRef, CWD: o.Facts.CWD, WorkspaceRoot: o.Facts.WorkspaceRoot,
		Slug: o.Facts.Slug, ShellTitle: o.Facts.ShellTitle, AdapterTitle: o.Facts.AdapterTitle,
		Subtitle: o.Facts.Subtitle, Command: o.Facts.Command, Remotes: o.Facts.Remotes,
		Working: o.Facts.Working, Unread: o.Facts.Unread, Error: o.Facts.Error,
		StartedAt: o.Facts.StartedAt, ExitedAt: o.Facts.ExitedAt, ExitCode: o.Facts.ExitCode,
		TerminalSize: o.Facts.TerminalSize,
	}
	return s.applyCommonFacts(ctx, o.ID, o.ObservedVersion, p, &o.ObservedAt)
}

func (s *Store) applyCommonFacts(ctx context.Context, id SessionID, observed RowVersion, p CommonFactsPatch, runnerObservedAt *UnixMillis) (MutationResult, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)
	raw, err := q.GetSession(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return MutationResult{}, ErrSessionNotFound
	}
	if err != nil {
		return MutationResult{}, err
	}
	v, err := sessionFromDB(raw)
	if err != nil {
		return MutationResult{}, err
	}
	if v.Version != observed {
		return MutationResult{SessionVersion: v.Version}, ErrStaleVersion
	}
	before := v
	if p.Adapter != nil {
		v.Adapter = *p.Adapter
	}
	if p.ConversationRef != nil {
		v.ConversationRef = *p.ConversationRef
	}
	if p.CWD != nil {
		v.CWD = *p.CWD
	}
	if p.WorkspaceRoot != nil {
		v.WorkspaceRoot = *p.WorkspaceRoot
	}
	if p.Slug != nil {
		v.Slug = *p.Slug
	}
	if p.ShellTitle != nil {
		v.ShellTitle = *p.ShellTitle
	}
	if p.AdapterTitle != nil {
		v.AdapterTitle = *p.AdapterTitle
	}
	if p.Subtitle != nil {
		v.Subtitle = *p.Subtitle
	}
	if p.Command != nil {
		v.Command = *p.Command
	}
	if p.Remotes != nil {
		v.Remotes = *p.Remotes
	}
	if p.Working != nil {
		v.Working = *p.Working
	}
	if p.Unread != nil {
		v.Unread = *p.Unread
	}
	if p.Error != nil {
		v.Error = *p.Error
	}
	// Sticky status-reported fact: any patch carrying a working/error fact
	// proves a status was reported for this row (runner-authoritative; the
	// acknowledgement path uses its own query and never sets it).
	v.StatusReported = v.StatusReported || p.Working != nil || p.Error != nil
	// last_output_at semantics (wire name; column keeps last_activity_at_ms):
	// bumped on (and only on) the unread false→true transition — the moment
	// the session produced output the user hasn't seen. Deliberately NOT
	// bumped by working/error transitions or exit; see store.Session
	// LastOutputAt docstring for the rationale (activity-feed sort key).
	if runnerObservedAt != nil && !before.Unread && v.Unread {
		if v.LastActivityAt == nil || *runnerObservedAt > *v.LastActivityAt {
			x := *runnerObservedAt
			v.LastActivityAt = &x
		}
	}
	if err = applyNullable(&v.StartedAt, p.StartedAt); err != nil {
		return MutationResult{}, err
	}
	if err = applyNullable(&v.ExitedAt, p.ExitedAt); err != nil {
		return MutationResult{}, err
	}
	if err = applyNullable(&v.LastActivityAt, p.LastActivityAt); err != nil {
		return MutationResult{}, err
	}
	if err = applyNullable(&v.ExitCode, p.ExitCode); err != nil {
		return MutationResult{}, err
	}
	if p.TerminalSize.Set != nil && p.TerminalSize.Clear {
		return MutationResult{}, errors.New("centralstore: terminal patch cannot set and clear")
	}
	if p.TerminalSize.Clear {
		v.TerminalCols = nil
		v.TerminalRows = nil
	} else if p.TerminalSize.Set != nil {
		size := *p.TerminalSize.Set
		if size.Cols == 0 || size.Rows == 0 {
			return MutationResult{}, errors.New("centralstore: terminal dimensions must be positive")
		}
		v.TerminalCols = &size.Cols
		v.TerminalRows = &size.Rows
	}
	for name, x := range map[string]*UnixMillis{"started timestamp": v.StartedAt, "exited timestamp": v.ExitedAt, "activity timestamp": v.LastActivityAt} {
		if err = validateMillis(name, x); err != nil {
			return MutationResult{}, err
		}
	}
	if v.Adapter == "" {
		return MutationResult{}, errors.New("centralstore: adapter required")
	}
	v.Title = ""
	before.Title = ""
	if reflect.DeepEqual(v, before) {
		if err = tx.Commit(); err != nil {
			return MutationResult{}, err
		}
		return MutationResult{SessionVersion: observed}, nil
	}
	cmd, rem, err := marshalWhole(v.Command, v.Remotes)
	if err != nil {
		return MutationResult{}, err
	}
	n, err := q.UpdateCommonFacts(ctx, db.UpdateCommonFactsParams{Adapter: v.Adapter, ConversationRef: nullString(v.ConversationRef), CommandJson: cmd, Cwd: v.CWD, WorkspaceRoot: nullString(v.WorkspaceRoot), RemotesJson: rem, Slug: nullString(v.Slug), ShellTitle: nullString(v.ShellTitle), AdapterTitle: nullString(v.AdapterTitle), Subtitle: nullString(v.Subtitle), Working: boolInt(v.Working), Unread: boolInt(v.Unread), HasError: boolInt(v.Error), StatusReported: boolInt(v.StatusReported), StartedAtMs: nullMillis(v.StartedAt), ExitedAtMs: nullMillis(v.ExitedAt), LastActivityAtMs: nullMillis(v.LastActivityAt), ExitCode: nullInt(v.ExitCode), TerminalCols: nullUint(v.TerminalCols), TerminalRows: nullUint(v.TerminalRows), ID: string(id), RowVersion: int64(observed)})
	if err != nil {
		return MutationResult{}, err
	}
	if n != 1 {
		return MutationResult{}, ErrStaleVersion
	}
	if err = tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Changed: true, SessionVersion: observed + 1, SessionsDirty: true}, nil
}

// AcknowledgeDeadSession clears the user-facing unread and error indicators.
// Runner liveness is deliberately outside SQLite: the lifecycle coordinator
// must establish that no runner is live immediately before calling this
// conditional operation.
func (s *Store) AcknowledgeDeadSession(ctx context.Context, id SessionID, observed RowVersion) (MutationResult, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)
	raw, err := q.GetSession(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return MutationResult{}, ErrSessionNotFound
	}
	if err != nil {
		return MutationResult{}, err
	}
	current, err := sessionFromDB(raw)
	if err != nil {
		return MutationResult{}, err
	}
	if current.Version != observed {
		return MutationResult{SessionVersion: current.Version}, ErrStaleVersion
	}
	if !current.Unread && !current.Error {
		if err = tx.Commit(); err != nil {
			return MutationResult{}, err
		}
		return MutationResult{SessionVersion: observed}, nil
	}
	n, err := q.AcknowledgeSessionAtVersion(ctx, db.AcknowledgeSessionAtVersionParams{ID: string(id), RowVersion: int64(observed)})
	if err != nil {
		return MutationResult{}, err
	}
	if n != 1 {
		return MutationResult{SessionVersion: current.Version}, ErrStaleVersion
	}
	if err = tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Changed: true, SessionVersion: observed + 1, SessionsDirty: true}, nil
}

func (s *Store) RemoveSessionAtVersion(ctx context.Context, id SessionID, observed RowVersion) (MutationResult, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)
	ver, err := q.SessionVersion(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return MutationResult{}, ErrSessionNotFound
	}
	if err != nil {
		return MutationResult{}, err
	}
	if RowVersion(ver) != observed {
		return MutationResult{SessionVersion: RowVersion(ver)}, ErrStaleVersion
	}
	if _, err = q.ClearDirectChildParents(ctx, nullString(string(id))); err != nil {
		return MutationResult{}, err
	}
	n, err := q.DeleteSessionAtVersion(ctx, db.DeleteSessionAtVersionParams{ID: string(id), RowVersion: ver})
	if err != nil {
		return MutationResult{}, err
	}
	if n != 1 {
		return MutationResult{SessionVersion: RowVersion(ver)}, ErrStaleVersion
	}
	if _, err = normalizePlacements(ctx, q, s.beforePlacementFinalize); err != nil {
		return MutationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Changed: true, SessionVersion: observed, SessionsDirty: true, WorldDirty: true}, nil
}

func normalizeRule(r MatchRule) (MatchRule, error) {
	hasP, hasR := r.Path != "", r.Remote != ""
	if hasP == hasR {
		return MatchRule{}, errors.New("centralstore: rule must contain exactly one path or remote")
	}
	if hasP {
		r.Path = filepath.Clean(r.Path)
		if r.Path == "." {
			return MatchRule{}, errors.New("centralstore: empty normalized path")
		}
	} else if r.Exact {
		return MatchRule{}, errors.New("centralstore: exact is valid only for path rules")
	}
	return r, nil
}
func normalizeSpecs(in []ProjectEntrySpec) ([]ProjectEntrySpec, error) {
	out := make([]ProjectEntrySpec, len(in))
	seenID := map[ProjectEntryID]bool{}
	seenPath := map[string]bool{}
	for i, e := range in {
		if e.ID < 0 {
			return nil, errors.New("centralstore: invalid project id")
		}
		if e.ID != 0 {
			if seenID[e.ID] {
				return nil, errors.New("centralstore: duplicate project id")
			}
			seenID[e.ID] = true
		}
		if (e.Owned == nil) == (e.Reference == nil) {
			return nil, errors.New("centralstore: project entry must be exactly owned or reference")
		}
		if e.Owned != nil {
			if e.Owned.Slug == "" || len(e.Owned.Rules) == 0 {
				return nil, errors.New("centralstore: owned project requires slug and rules")
			}
			x := *e.Owned
			x.Rules = make([]MatchRule, len(e.Owned.Rules))
			for j, r := range e.Owned.Rules {
				n, err := normalizeRule(r)
				if err != nil {
					return nil, err
				}
				if n.Path != "" {
					if seenPath[n.Path] {
						return nil, errors.New("centralstore: duplicate normalized path")
					}
					seenPath[n.Path] = true
				}
				x.Rules[j] = n
			}
			e.Owned = &x
		} else if e.Reference.PeerKey == "" || e.Reference.Slug == "" {
			return nil, errors.New("centralstore: reference requires peer key and slug")
		}
		out[i] = e
	}
	return out, nil
}
func specEntry(e ProjectEntrySpec) ProjectEntry {
	if e.Owned != nil {
		return ProjectEntry{ID: e.ID, Kind: ProjectEntryOwned, Slug: e.Owned.Slug, Rules: e.Owned.Rules}
	}
	return ProjectEntry{ID: e.ID, Kind: ProjectEntryReference, Slug: e.Reference.Slug, PeerKey: e.Reference.PeerKey, NodeID: e.Reference.NodeID}
}
func catalogFromQueries(ctx context.Context, q *db.Queries) (ProjectCatalog, error) {
	entries, err := q.ListProjectEntries(ctx)
	if err != nil {
		return nil, err
	}
	rules, err := q.ListProjectRules(ctx)
	if err != nil {
		return nil, err
	}
	by := map[int64][]MatchRule{}
	for _, r := range rules {
		if r.Exact != 0 && r.Exact != 1 {
			return nil, errors.New("centralstore: corrupt project rule boolean")
		}
		rule := MatchRule{Path: r.Path.String, Remote: r.Remote.String, Exact: r.Exact == 1}
		if _, e := normalizeRule(rule); e != nil {
			return nil, fmt.Errorf("centralstore: corrupt project rule: %w", e)
		}
		by[r.ProjectEntryID] = append(by[r.ProjectEntryID], rule)
	}
	out := make(ProjectCatalog, 0, len(entries))
	for _, e := range entries {
		x := ProjectEntry{ID: ProjectEntryID(e.ID), Kind: ProjectEntryKind(e.EntryKind), Slug: e.Slug, PeerKey: PeerKey(e.PeerKey.String), NodeID: e.NodeID.String, Rules: by[e.ID], CreatedAt: UnixMillis(e.CreatedAtMs), UpdatedAt: UnixMillis(e.UpdatedAtMs)}
		if e.ID <= 0 || e.SidebarOrder < 0 || e.CreatedAtMs < 0 || e.UpdatedAtMs < 0 || x.Slug == "" {
			return nil, errors.New("centralstore: corrupt project value")
		}
		if x.Kind != ProjectEntryOwned && x.Kind != ProjectEntryReference {
			return nil, errors.New("centralstore: corrupt project kind")
		}
		if x.Kind == ProjectEntryOwned && (x.PeerKey != "" || x.NodeID != "" || len(x.Rules) == 0) {
			return nil, errors.New("centralstore: corrupt owned project")
		}
		if x.Kind == ProjectEntryReference && (x.PeerKey == "" || len(x.Rules) != 0) {
			return nil, errors.New("centralstore: corrupt project reference")
		}
		out = append(out, x)
	}
	return out, nil
}
func (s *Store) ListProjectCatalog(ctx context.Context) (ProjectCatalog, error) {
	return catalogFromQueries(ctx, s.queries)
}

func assertCatalogOrder(ctx context.Context, q *db.Queries) error {
	entries, err := q.ListProjectEntries(ctx)
	if err != nil {
		return err
	}
	for i, entry := range entries {
		if entry.SidebarOrder != int64(i) {
			return errors.New("centralstore: project order is not dense")
		}
	}
	rules, err := q.ListProjectRules(ctx)
	if err != nil {
		return err
	}
	next := map[int64]int64{}
	for _, rule := range rules {
		if rule.RuleOrder != next[rule.ProjectEntryID] {
			return errors.New("centralstore: project rule order is not dense")
		}
		next[rule.ProjectEntryID]++
	}
	return nil
}

func sameProjectShape(a, b ProjectEntry) bool {
	return a.ID == b.ID && a.Kind == b.Kind && a.Slug == b.Slug && a.PeerKey == b.PeerKey && a.NodeID == b.NodeID && reflect.DeepEqual(a.Rules, b.Rules)
}

// ReplaceProjectCatalog is a bootstrap ordering primitive, not an
// authoritative project-membership operation. It is deliberately restricted
// to stores with zero placements because it carries no match inputs with
// which to rematch local and connected Local-peer subjects; use
// ReplaceProjectCatalogAndRematch once placement exists.
func (s *Store) ReplaceProjectCatalog(ctx context.Context, input []ProjectEntrySpec, at UnixMillis) (ProjectCatalog, MutationResult, error) {
	if at < 0 {
		return nil, MutationResult{}, errors.New("centralstore: catalog timestamp must be non-negative")
	}
	in, err := normalizeSpecs(input)
	if err != nil {
		return nil, MutationResult{}, err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)
	placementCount, err := q.PlacementCount(ctx)
	if err != nil {
		return nil, MutationResult{}, err
	}
	if placementCount != 0 {
		return nil, MutationResult{}, ErrCatalogHasPlacements
	}
	out, changed, err := replaceCatalogInTx(ctx, q, in, at)
	if err != nil {
		return nil, MutationResult{}, err
	}
	if !changed {
		if err = tx.Commit(); err != nil {
			return nil, MutationResult{}, err
		}
		return out, MutationResult{}, nil
	}
	if count, e := q.PlacementCount(ctx); e != nil {
		return nil, MutationResult{}, e
	} else if count != 0 {
		return nil, MutationResult{}, errors.New("centralstore: placement appeared during bootstrap catalog replacement")
	}
	if err = tx.Commit(); err != nil {
		return nil, MutationResult{}, err
	}
	return out, MutationResult{Changed: true, WorldDirty: true}, nil
}

// replaceCatalogInTx applies one normalized catalog specification inside an
// open transaction: identity-immutability validation, order/slug parking,
// removal, insertion, in-place updates, and rule-set replacement. It reports
// changed=false (and performs no write) when the specification is identical
// to the stored catalog. Placement consequences are the caller's business.
func replaceCatalogInTx(ctx context.Context, q *db.Queries, in []ProjectEntrySpec, at UnixMillis) (ProjectCatalog, bool, error) {
	current, err := catalogFromQueries(ctx, q)
	if err != nil {
		return nil, false, err
	}
	byID := map[ProjectEntryID]ProjectEntry{}
	oldIndex := map[ProjectEntryID]int{}
	for i, e := range current {
		byID[e.ID] = e
		oldIndex[e.ID] = i
	}
	changedEntry := map[ProjectEntryID]bool{}
	changedRules := map[ProjectEntryID]bool{}
	unchanged := len(current) == len(in)
	for i, e := range in {
		want := specEntry(e)
		if e.ID != 0 {
			old, ok := byID[e.ID]
			if !ok {
				return nil, false, errors.New("centralstore: unknown project id")
			}
			// A reference entry's slug is half its identity (ADR 0026 §7:
			// references are identified by peer identity plus remote slug), so
			// rebinding it in place is rejected; use delete+insert instead.
			if old.Kind != want.Kind || old.PeerKey != want.PeerKey ||
				(want.Kind == ProjectEntryReference && old.Slug != want.Slug) {
				return nil, false, errors.New("centralstore: project identity is immutable")
			}
			changedEntry[e.ID] = oldIndex[e.ID] != i || old.Slug != want.Slug || old.NodeID != want.NodeID || !reflect.DeepEqual(old.Rules, want.Rules)
			changedRules[e.ID] = !reflect.DeepEqual(old.Rules, want.Rules)
			unchanged = unchanged && oldIndex[e.ID] == i && sameProjectShape(old, want)
		} else {
			unchanged = false
		}
	}
	if unchanged {
		return current, false, nil
	}
	if len(current) > 0 {
		offset := int64(len(current) + len(in) + 1)
		if err = q.ParkProjectEntries(ctx, offset); err != nil {
			return nil, false, err
		}
	}
	// Delete removed entries and park changed entries' slugs before the
	// insert/update loop so slug swaps and delete-plus-re-add of the same
	// slug cannot collide with the partial unique slug indexes
	// mid-transaction, mirroring the sidebar-order and rule-path treatment.
	keep := map[ProjectEntryID]bool{}
	for _, e := range in {
		if e.ID != 0 {
			keep[e.ID] = true
		}
	}
	for _, e := range current {
		if keep[e.ID] {
			continue
		}
		n, er := q.DeleteProjectEntry(ctx, int64(e.ID))
		if er != nil {
			return nil, false, er
		}
		if n != 1 {
			return nil, false, errors.New("centralstore: project delete lost identity")
		}
	}
	slugNonce, err := nonce()
	if err != nil {
		return nil, false, err
	}
	parkedSlugs := 0
	for _, e := range in {
		if e.ID == 0 || byID[e.ID].Slug == specEntry(e).Slug {
			continue
		}
		n, er := q.ParkProjectEntrySlug(ctx, db.ParkProjectEntrySlugParams{Slug: fmt.Sprintf("~:%s:%d", slugNonce, parkedSlugs), ID: int64(e.ID)})
		if er != nil {
			return nil, false, er
		}
		if n != 1 {
			return nil, false, errors.New("centralstore: project slug parking lost identity")
		}
		parkedSlugs++
	}
	for i, e := range in {
		entry := specEntry(e)
		if e.ID == 0 {
			id, er := q.InsertProjectEntry(ctx, db.InsertProjectEntryParams{SidebarOrder: int64(i), EntryKind: string(entry.Kind), Slug: entry.Slug, PeerKey: nullString(string(entry.PeerKey)), NodeID: nullString(entry.NodeID), CreatedAtMs: int64(at), UpdatedAtMs: int64(at)})
			if er != nil {
				return nil, false, er
			}
			e.ID = ProjectEntryID(id)
			in[i].ID = e.ID
			changedEntry[e.ID] = true
			changedRules[e.ID] = e.Owned != nil
		} else if changedEntry[e.ID] {
			n, er := q.UpdateProjectEntry(ctx, db.UpdateProjectEntryParams{SidebarOrder: int64(i), Slug: entry.Slug, NodeID: nullString(entry.NodeID), UpdatedAtMs: int64(at), ID: int64(e.ID)})
			if er != nil {
				return nil, false, er
			}
			if n != 1 {
				return nil, false, errors.New("centralstore: project update lost identity")
			}
		} else {
			n, er := q.FinalizeProjectEntryOrder(ctx, db.FinalizeProjectEntryOrderParams{SidebarOrder: int64(i), ID: int64(e.ID)})
			if er != nil {
				return nil, false, er
			}
			if n != 1 {
				return nil, false, errors.New("centralstore: project order finalization lost identity")
			}
		}
	}
	// Delete all changed rule sets before inserting any replacements so valid
	// path swaps cannot collide with the partial unique index mid-transaction.
	for _, e := range in {
		if e.Owned != nil && changedRules[e.ID] {
			if err = q.DeleteProjectRules(ctx, int64(e.ID)); err != nil {
				return nil, false, err
			}
		}
	}
	for _, e := range in {
		if e.Owned == nil || !changedRules[e.ID] {
			continue
		}
		for j, r := range e.Owned.Rules {
			if err = q.InsertProjectRule(ctx, db.InsertProjectRuleParams{ProjectEntryID: int64(e.ID), RuleOrder: int64(j), Path: nullString(r.Path), Remote: nullString(r.Remote), Exact: boolInt(r.Exact)}); err != nil {
				return nil, false, err
			}
		}
	}
	out, err := catalogFromQueries(ctx, q)
	if err != nil {
		return nil, false, err
	}
	if len(out) != len(in) {
		return nil, false, errors.New("centralstore: project cardinality invariant failed")
	}
	if err = assertCatalogOrder(ctx, q); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func subjectKey(s SubjectRef) string {
	if s.LocalSessionID != "" {
		return "l:" + string(s.LocalSessionID)
	}
	if s.LocalPeer != nil {
		return "p:" + escape(string(s.LocalPeer.PeerKey)) + ":" + escape(s.LocalPeer.SessionID)
	}
	return ""
}
func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "%", "%25"), ":", "%3A")
}

type scopeKey struct {
	project int64
	scope   string
}
type placementRec struct {
	id, project                         int64
	local, peer, session, parent, scope string
	pos                                 int64
	created                             int64
	promoted                            bool
	oldProject                          int64
	oldScope                            string
	oldPos                              int64
	isNew                               bool
}

func placements(ctx context.Context, q *db.Queries) ([]*placementRec, error) {
	rows, err := q.ListPlacements(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*placementRec, 0, len(rows))
	for _, x := range rows {
		localArm := x.LocalSessionID.Valid
		peerArm := x.LocalPeerKey.Valid && x.PeerSessionID.Valid
		if x.ID <= 0 || x.ProjectEntryID <= 0 || x.Position < 0 || x.SiblingScope == "" || localArm == peerArm || x.LocalPromotedToRoot < 0 || x.LocalPromotedToRoot > 1 {
			return nil, errors.New("centralstore: corrupt placement value")
		}
		if localArm && (x.LocalSessionID.String == "" || x.LocalCreatedAtMs < 0) {
			return nil, errors.New("centralstore: corrupt local placement")
		}
		if peerArm && (x.LocalPeerKey.String == "" || x.PeerSessionID.String == "") {
			return nil, errors.New("centralstore: corrupt Local-peer placement")
		}
		r := &placementRec{id: x.ID, project: x.ProjectEntryID, local: x.LocalSessionID.String, peer: x.LocalPeerKey.String, session: x.PeerSessionID.String, parent: x.PeerParentSessionID.String, scope: x.SiblingScope, pos: x.Position, created: x.LocalCreatedAtMs, promoted: x.LocalPromotedToRoot == 1, oldProject: x.ProjectEntryID, oldScope: x.SiblingScope, oldPos: x.Position}
		if r.local != "" {
			r.parent = x.LaunchParentID.String
		}
		out = append(out, r)
	}
	return out, nil
}
func recKey(r *placementRec) string {
	if r.local != "" {
		return "l:" + r.local
	}
	return "p:" + escape(r.peer) + ":" + escape(r.session)
}
func desiredScope(all []*placementRec, r *placementRec) string {
	if r.promoted {
		return "r"
	}
	parentKey := ""
	if r.local != "" && r.parent != "" {
		parentKey = "l:" + r.parent
	} else if r.local == "" && r.parent != "" {
		parentKey = "p:" + escape(r.peer) + ":" + escape(r.parent)
	}
	if parentKey == "" {
		return "r"
	}
	for _, p := range all {
		if p.project == r.project && recKey(p) == parentKey {
			return "c:" + parentKey
		}
	}
	return "r"
}
func nonce() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func normalizePlacements(ctx context.Context, q *db.Queries, fault func() error) (bool, error) {
	all, err := placements(ctx, q)
	if err != nil {
		return false, err
	}
	return rewritePlacements(ctx, q, all, nil, fault)
}
func rewritePlacements(ctx context.Context, q *db.Queries, all []*placementRec, explicit map[scopeKey][]*placementRec, fault func() error) (bool, error) {
	final := map[scopeKey][]*placementRec{}
	affected := map[scopeKey]bool{}
	for _, r := range all {
		newScope := desiredScope(all, r)
		old := scopeKey{r.oldProject, r.oldScope}
		next := scopeKey{r.project, newScope}
		if r.isNew || old != next {
			if !r.isNew {
				affected[old] = true
			}
			affected[next] = true
		}
		r.scope = newScope
		final[next] = append(final[next], r)
	}
	for k, g := range final {
		sort.SliceStable(g, func(i, j int) bool {
			a, b := g[i], g[j]
			am := a.isNew || a.oldProject != a.project || a.oldScope != a.scope
			bm := b.isNew || b.oldProject != b.project || b.oldScope != b.scope
			if am != bm {
				return !am
			}
			if am {
				if a.local != "" && b.local != "" {
					if a.created != b.created {
						return a.created < b.created
					}
					return a.local < b.local
				}
				return recKey(a) < recKey(b)
			}
			if a.oldPos != b.oldPos {
				return a.oldPos < b.oldPos
			}
			return a.id < b.id
		})
		for i, r := range g {
			if r.oldProject == r.project && r.oldScope == r.scope && r.oldPos != int64(i) {
				affected[k] = true
			}
		}
	}
	for k, g := range explicit {
		current := final[k]
		if len(g) != len(current) {
			return false, errors.New("centralstore: explicit order does not contain every sibling")
		}
		seen := map[*placementRec]bool{}
		same := true
		for i, r := range g {
			if seen[r] {
				return false, errors.New("centralstore: duplicate reorder subject")
			}
			seen[r] = true
			same = same && current[i] == r
		}
		final[k] = g
		if !same {
			affected[k] = true
		}
	}
	if len(affected) == 0 {
		return false, nil
	}
	park := map[int64]*placementRec{}
	for _, r := range all {
		if r.id != 0 && (affected[scopeKey{r.oldProject, r.oldScope}] || affected[scopeKey{r.project, r.scope}]) {
			park[r.id] = r
		}
	}
	n, err := nonce()
	if err != nil {
		return false, err
	}
	ids := make([]int64, 0, len(park))
	for id := range park {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i, id := range ids {
		count, e := q.ParkPlacement(ctx, db.ParkPlacementParams{SiblingScope: fmt.Sprintf("~:%s:%d", n, i), ID: id})
		if e != nil {
			return false, e
		}
		if count != 1 {
			return false, errors.New("centralstore: placement disappeared while parking")
		}
	}
	if fault != nil {
		if err := fault(); err != nil {
			return false, err
		}
	}
	keys := make([]scopeKey, 0, len(affected))
	for k := range affected {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].project != keys[j].project {
			return keys[i].project < keys[j].project
		}
		return keys[i].scope < keys[j].scope
	})
	for _, k := range keys {
		for i, r := range final[k] {
			if r.id == 0 {
				if r.local != "" {
					r.id, err = q.InsertLocalPlacement(ctx, db.InsertLocalPlacementParams{ProjectEntryID: r.project, LocalSessionID: nullString(r.local), SiblingScope: k.scope, Position: int64(i)})
				} else {
					r.id, err = q.InsertLocalPeerPlacement(ctx, db.InsertLocalPeerPlacementParams{ProjectEntryID: r.project, LocalPeerKey: nullString(r.peer), PeerSessionID: nullString(r.session), PeerParentSessionID: nullString(r.parent), SiblingScope: k.scope, Position: int64(i)})
				}
				if err != nil {
					return false, err
				}
				r.isNew = false
			} else if r.local != "" {
				var count int64
				count, err = q.FinalizeLocalPlacement(ctx, db.FinalizeLocalPlacementParams{ProjectEntryID: r.project, SiblingScope: k.scope, Position: int64(i), ID: r.id})
				if err == nil && count != 1 {
					err = errors.New("centralstore: local placement disappeared")
				}
			} else {
				var count int64
				count, err = q.FinalizeLocalPeerPlacement(ctx, db.FinalizeLocalPeerPlacementParams{ProjectEntryID: r.project, PeerParentSessionID: nullString(r.parent), SiblingScope: k.scope, Position: int64(i), ID: r.id})
				if err == nil && count != 1 {
					err = errors.New("centralstore: peer placement disappeared")
				}
			}
			if err != nil {
				return false, err
			}
		}
	}
	if count, err := q.TemporaryPlacementCount(ctx); err != nil {
		return false, err
	} else if count != 0 {
		return false, errors.New("centralstore: temporary placement scope remained")
	}
	check, err := placements(ctx, q)
	if err != nil {
		return false, err
	}
	groups := map[scopeKey][]int64{}
	for _, r := range check {
		k := scopeKey{r.project, r.scope}
		groups[k] = append(groups[k], r.pos)
	}
	for k, p := range groups {
		sort.Slice(p, func(i, j int) bool { return p[i] < p[j] })
		for i, x := range p {
			if x != int64(i) {
				return false, fmt.Errorf("centralstore: non-dense placement scope %d/%s", k.project, k.scope)
			}
		}
	}
	return true, nil
}

func (s *Store) PlaceLocalSession(ctx context.Context, id SessionID, project ProjectEntryID) (MutationResult, error) {
	return s.place(ctx, SubjectRef{LocalSessionID: id}, project)
}
func (s *Store) UpsertLocalPeerPlacement(ctx context.Context, p LocalPeerSubject, project ProjectEntryID) (MutationResult, error) {
	return s.place(ctx, SubjectRef{LocalPeer: &p}, project)
}
func validateSubject(sub SubjectRef) error {
	if (sub.LocalSessionID == "") == (sub.LocalPeer == nil) {
		return errors.New("centralstore: subject must be exactly local or Local-peer")
	}
	if sub.LocalPeer != nil && (sub.LocalPeer.PeerKey == "" || sub.LocalPeer.SessionID == "") {
		return errors.New("centralstore: invalid Local-peer subject")
	}
	if sub.LocalPeer != nil && sub.LocalPeer.ParentSessionID == sub.LocalPeer.SessionID {
		return ErrLocalPeerParentCycle
	}
	return nil
}

func validateLocalPeerParentGraph(all []*placementRec) error {
	parents := map[string]string{}
	for _, r := range all {
		if r.local != "" || r.parent == "" {
			continue
		}
		parents[recKey(r)] = "p:" + escape(r.peer) + ":" + escape(r.parent)
	}
	state := map[string]uint8{}
	var visit func(string) bool
	visit = func(key string) bool {
		switch state[key] {
		case 1:
			return true
		case 2:
			return false
		}
		state[key] = 1
		if parent, ok := parents[key]; ok && visit(parent) {
			return true
		}
		state[key] = 2
		return false
	}
	for key := range parents {
		if visit(key) {
			return ErrLocalPeerParentCycle
		}
	}
	return nil
}

func (s *Store) place(ctx context.Context, sub SubjectRef, project ProjectEntryID) (MutationResult, error) {
	if err := validateSubject(sub); err != nil {
		return MutationResult{}, err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)
	owned, err := q.OwnedProjectExists(ctx, int64(project))
	if err != nil {
		return MutationResult{}, err
	}
	if !owned {
		return MutationResult{}, errors.New("centralstore: placement requires owned project")
	}
	all, err := placements(ctx, q)
	if err != nil {
		return MutationResult{}, err
	}
	key := subjectKey(sub)
	var target *placementRec
	for _, r := range all {
		if recKey(r) == key {
			target = r
			break
		}
	}
	if sub.LocalSessionID != "" {
		facts, e := q.LocalSessionPlacementFacts(ctx, string(sub.LocalSessionID))
		if e != nil {
			return MutationResult{}, e
		}
		if target == nil {
			target = &placementRec{project: int64(project), local: string(sub.LocalSessionID), parent: facts.LaunchParentID.String, created: facts.CreatedAtMs, promoted: facts.PromotedToRoot == 1, isNew: true}
			all = append(all, target)
		}
	} else {
		p := sub.LocalPeer
		if target == nil {
			target = &placementRec{project: int64(project), peer: string(p.PeerKey), session: p.SessionID, parent: p.ParentSessionID, isNew: true}
			all = append(all, target)
		}
	}
	oldProject, oldParent := target.project, target.parent
	target.project = int64(project)
	if sub.LocalPeer != nil {
		target.parent = sub.LocalPeer.ParentSessionID
		if err = validateLocalPeerParentGraph(all); err != nil {
			return MutationResult{}, err
		}
	}
	scope := desiredScope(all, target)
	if !target.isNew && oldProject == target.project && oldParent == target.parent && target.oldScope == scope {
		if err = tx.Commit(); err != nil {
			return MutationResult{}, err
		}
		return MutationResult{}, nil
	}
	changed, err := rewritePlacements(ctx, q, all, nil, s.beforePlacementFinalize)
	if err != nil {
		return MutationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Changed: changed, SessionsDirty: changed && sub.LocalSessionID != "", WorldDirty: changed}, nil
}

type SiblingReorder struct {
	Project ProjectEntryID
	Parent  ParentRef
	Order   []SubjectRef
}

func (s *Store) ReorderSiblings(ctx context.Context, project ProjectEntryID, parent ParentRef, order []SubjectRef) (MutationResult, error) {
	return s.ReorderSiblingScopes(ctx, []SiblingReorder{{Project: project, Parent: parent, Order: order}})
}

// ReorderSiblingScopes applies all requested scopes in one transaction.
func (s *Store) ReorderSiblingScopes(ctx context.Context, reorders []SiblingReorder) (MutationResult, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)
	changedAny := false
	for _, reorder := range reorders {
		project, parent, order := reorder.Project, reorder.Parent, reorder.Order

		owned, err := q.OwnedProjectExists(ctx, int64(project))
		if err != nil {
			return MutationResult{}, err
		}
		if !owned {
			return MutationResult{}, errors.New("centralstore: reorder requires an owned project")
		}
		all, err := placements(ctx, q)
		if err != nil {
			return MutationResult{}, err
		}
		scope := "r"
		if parent.Subject != nil {
			if err = validateSubject(*parent.Subject); err != nil {
				return MutationResult{}, err
			}
			parentKey := subjectKey(*parent.Subject)
			found := false
			for _, r := range all {
				if r.project == int64(project) && recKey(r) == parentKey {
					found = true
					break
				}
			}
			if !found {
				return MutationResult{}, errors.New("centralstore: reorder parent is not placed in project")
			}
			scope = "c:" + parentKey
		}
		k := scopeKey{int64(project), scope}
		by := map[string]*placementRec{}
		var current []*placementRec
		for _, r := range all {
			if r.project == k.project && desiredScope(all, r) == scope {
				by[recKey(r)] = r
				current = append(current, r)
			}
		}
		if len(current) != len(order) {
			return MutationResult{}, errors.New("centralstore: reorder must contain every sibling")
		}
		desired := make([]*placementRec, len(order))
		seen := map[string]bool{}
		for i, sub := range order {
			if err = validateSubject(sub); err != nil {
				return MutationResult{}, err
			}
			key := subjectKey(sub)
			if seen[key] {
				return MutationResult{}, errors.New("centralstore: duplicate reorder subject")
			}
			seen[key] = true
			r, ok := by[key]
			if !ok {
				return MutationResult{}, errors.New("centralstore: reorder subject outside scope")
			}
			desired[i] = r
		}
		sort.Slice(current, func(i, j int) bool { return current[i].oldPos < current[j].oldPos })
		same := len(current) == len(desired)
		for i := range current {
			same = same && current[i] == desired[i]
		}
		if same {
			continue
		}
		changed, err := rewritePlacements(ctx, q, all, map[scopeKey][]*placementRec{k: desired}, s.beforePlacementFinalize)
		if err != nil {
			return MutationResult{}, err
		}
		changedAny = changedAny || changed
	}
	if err = tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Changed: changedAny, WorldDirty: changedAny}, nil
}

func (s *Store) SetPromotion(ctx context.Context, id SessionID, promoted bool, index *int) (MutationResult, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)
	ver, err := q.SessionVersion(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return MutationResult{}, ErrSessionNotFound
	}
	if err != nil {
		return MutationResult{}, err
	}
	n, err := q.SetPromotion(ctx, db.SetPromotionParams{PromotedToRoot: boolInt(promoted), ID: string(id), PromotedToRoot_2: boolInt(promoted)})
	if err != nil {
		return MutationResult{}, err
	}
	all, err := placements(ctx, q)
	if err != nil {
		return MutationResult{}, err
	}
	changed, err := rewritePlacements(ctx, q, all, nil, s.beforePlacementFinalize)
	if err != nil {
		return MutationResult{}, err
	}
	if index != nil {
		var target *placementRec
		for _, r := range all {
			if r.local == string(id) {
				target = r
				break
			}
		}
		if target == nil {
			return MutationResult{}, errors.New("centralstore: cannot position an unplaced session")
		}
		k := scopeKey{target.project, desiredScope(all, target)}
		var group []*placementRec
		for _, r := range all {
			if r.project == k.project && desiredScope(all, r) == k.scope && r != target {
				group = append(group, r)
			}
		}
		sort.Slice(group, func(i, j int) bool { return group[i].pos < group[j].pos })
		if *index < 0 || *index > len(group) {
			return MutationResult{}, errors.New("centralstore: invalid insertion index")
		}
		group = append(group, nil)
		copy(group[*index+1:], group[*index:])
		group[*index] = target
		orderChanged, er := rewritePlacements(ctx, q, all, map[scopeKey][]*placementRec{k: group}, s.beforePlacementFinalize)
		if er != nil {
			return MutationResult{}, er
		}
		changed = changed || orderChanged
	}
	if n == 0 && !changed {
		if err = tx.Commit(); err != nil {
			return MutationResult{}, err
		}
		return MutationResult{SessionVersion: RowVersion(ver)}, nil
	}
	if err = tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	version := RowVersion(ver)
	if n == 1 {
		version++
		changed = true
	}
	return MutationResult{Changed: changed, SessionVersion: version, SessionsDirty: n == 1, WorldDirty: changed}, nil
}

func (s *Store) PruneLocalPeer(ctx context.Context, key PeerKey) (MutationResult, error) {
	if key == "" {
		return MutationResult{}, errors.New("centralstore: peer key required")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)
	n, err := q.DeleteLocalPeerPlacements(ctx, nullString(string(key)))
	if err != nil {
		return MutationResult{}, err
	}
	if n > 0 {
		if _, err = normalizePlacements(ctx, q, s.beforePlacementFinalize); err != nil {
			return MutationResult{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Changed: n > 0, WorldDirty: n > 0}, nil
}
