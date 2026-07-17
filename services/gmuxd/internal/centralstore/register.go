package centralstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore/internal/db"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/projectmatch"
)

// RunnerRegistration is the complete observation used by the nonproduction
// registration transaction. I/O and liveness proof happen before this call.
type RunnerRegistration struct {
	ID      SessionID
	Adapter string
	Alive   bool
	// NewGeneration is coordinator provenance: for an existing row, this
	// observation belongs to a newly reserved replacement/resume/restart
	// generation rather than a startup re-observation of the same runner.
	// The singleton coordinator must serialize lifecycle operations and prove
	// its reservation is still current immediately before this call. The
	// generation itself remains runtime-only and is never persisted.
	NewGeneration  bool
	CreatedAt      UnixMillis
	LaunchParentID *SessionID
	ObservedAt     UnixMillis
	Facts          RunnerFacts
	// Evict is the conversation-takeover loser set: dead retained rows whose
	// conversation the registering live runner covers (same opaque ref, or an
	// ancestor per the adapter's lineage — resolved by the coordinator, which
	// owns liveness and adapter I/O; SQLite never knows either). Each eviction
	// is applied conditionally at its observed row version inside this same
	// transaction (ADR 0026 §9 "conversation-lineage takeover plus placement
	// cleanup"): a stale or vanished loser is skipped, never fatal — the
	// registration must land and a later reconciliation pass converges the
	// leftover. Deletion consequences match RemoveSessionAtVersion: the
	// loser's direct children become genuine roots and every affected sibling
	// scope is renormalized. Evictions are only legal on a live registration
	// (only a live binder takes over) and never target the registering ID.
	Evict []TakeoverEviction
}

// TakeoverEviction is one conditional conversation-takeover deletion.
type TakeoverEviction struct {
	ID      SessionID
	Version RowVersion
}

// RunnerFacts is tri-state: nil/zero patch means unobserved, a pointer stores
// its value (including an empty value), and NullablePatch can set or clear SQL
// nullable facts. Live registration always clears exit facts.
type RunnerFacts struct {
	ConversationRef, CWD, WorkspaceRoot, Slug, ShellTitle, AdapterTitle, Subtitle *string
	Command                                                                       *[]string
	Remotes                                                                       *map[string]string
	Working, Unread, Error                                                        *bool
	StartedAt, ExitedAt                                                           NullablePatch[UnixMillis]
	ExitCode                                                                      NullablePatch[int]
	TerminalSize                                                                  NullablePatch[TerminalSize]
}

var (
	ErrAdapterMismatch = errors.New("centralstore: runner adapter mismatch")
	// ErrInvalidEviction marks a takeover eviction that is structurally
	// illegal: an eviction on a dead registration (only a live binder may
	// take over), an empty loser ID, or a self-eviction.
	ErrInvalidEviction           = errors.New("centralstore: invalid takeover eviction")
	ErrGenerationExitRequired    = errors.New("centralstore: new dead runner generation requires exited timestamp")
	ErrUnexpectedGenerationClaim = errors.New("centralstore: new generation claim requires an existing row")
)

// RegisterRunner atomically merges one runner observation with durable
// history and derived project placement. It is nonproduction infrastructure;
// callers must perform all I/O first. Immediately before calling, the
// singleton lifecycle coordinator must hold serialization, validate that its
// reserved runtime generation is still current, and set NewGeneration exactly
// when replacing an existing row's generation. Generation is not persisted.
func (s *Store) RegisterRunner(ctx context.Context, reg RunnerRegistration) (Session, MutationResult, error) {
	if reg.ID == "" || reg.Adapter == "" {
		return Session{}, MutationResult{}, errors.New("centralstore: session id and adapter required")
	}
	if reg.CreatedAt < 0 || reg.ObservedAt < 0 {
		return Session{}, MutationResult{}, errors.New("centralstore: registration timestamps must be non-negative")
	}
	if reg.LaunchParentID != nil && (*reg.LaunchParentID == "" || *reg.LaunchParentID == reg.ID) {
		return Session{}, MutationResult{}, errors.New("centralstore: invalid launch parent")
	}
	if err := validateRunnerFacts(reg.Facts); err != nil {
		return Session{}, MutationResult{}, err
	}
	for _, ev := range reg.Evict {
		if !reg.Alive {
			return Session{}, MutationResult{}, fmt.Errorf("%w: eviction requires a live registration", ErrInvalidEviction)
		}
		if ev.ID == "" || ev.ID == reg.ID {
			return Session{}, MutationResult{}, fmt.Errorf("%w: loser id %q", ErrInvalidEviction, ev.ID)
		}
	}

	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)

	raw, err := q.GetSession(ctx, string(reg.ID))
	isNew := errors.Is(err, sql.ErrNoRows)
	if err != nil && !isNew {
		return Session{}, MutationResult{}, err
	}

	if isNew && reg.NewGeneration {
		return Session{}, MutationResult{}, ErrUnexpectedGenerationClaim
	}

	var before, current Session
	var rowChanged, dismissed bool
	if !isNew {
		// Adapter identity is checked before any generation-contract check so
		// a caller registering the wrong adapter sees the more fundamental
		// identity error.
		before, err = sessionFromDB(raw)
		if err != nil {
			return Session{}, MutationResult{}, err
		}
		if before.Adapter != reg.Adapter {
			return Session{}, MutationResult{SessionVersion: before.Version}, ErrAdapterMismatch
		}
	}
	// Every dead generation must carry an explicit exit timestamp: a
	// replacement generation because it must never inherit the previous
	// generation's exit facts, and a brand-new dead row because there is no
	// prior fact to fall back on.
	if (isNew || reg.NewGeneration) && !reg.Alive && reg.Facts.ExitedAt.Set == nil {
		return Session{}, MutationResult{}, ErrGenerationExitRequired
	}

	if isNew {
		current = Session{
			ID: reg.ID, Adapter: reg.Adapter, CreatedAt: reg.CreatedAt,
			Command: []string{}, Remotes: map[string]string{}, LaunchParentID: cloneSessionID(reg.LaunchParentID),
		}
		if err = mergeRunnerFacts(&current, reg.Facts); err != nil {
			return Session{}, MutationResult{}, err
		}
		if reg.Alive {
			current.ExitedAt, current.ExitCode = nil, nil
		}
		current.LastActivityAt = initialActivity(current.LastActivityAt, reg, false, Session{})
		if err = validateMergedSession(current); err != nil {
			return Session{}, MutationResult{}, err
		}
		cmd, rem, marshalErr := marshalWhole(current.Command, current.Remotes)
		if marshalErr != nil {
			return Session{}, MutationResult{}, marshalErr
		}
		raw, err = q.InsertSession(ctx, db.InsertSessionParams{
			ID: string(current.ID), Adapter: current.Adapter, ConversationRef: nullString(current.ConversationRef),
			CommandJson: cmd, Cwd: current.CWD, WorkspaceRoot: nullString(current.WorkspaceRoot), RemotesJson: rem,
			Slug: nullString(current.Slug), ShellTitle: nullString(current.ShellTitle), AdapterTitle: nullString(current.AdapterTitle), Subtitle: nullString(current.Subtitle),
			Working: boolInt(current.Working), Unread: boolInt(current.Unread), HasError: boolInt(current.Error), StatusReported: boolInt(current.StatusReported), CreatedAtMs: int64(current.CreatedAt),
			StartedAtMs: nullMillis(current.StartedAt), ExitedAtMs: nullMillis(current.ExitedAt), LastActivityAtMs: nullMillis(current.LastActivityAt), ExitCode: nullInt(current.ExitCode),
			TerminalCols: nullUint(current.TerminalCols), TerminalRows: nullUint(current.TerminalRows), LaunchParentID: func() sql.NullString {
				if current.LaunchParentID == nil {
					return sql.NullString{}
				}
				return nullString(string(*current.LaunchParentID))
			}(),
		})
		if err != nil {
			return Session{}, MutationResult{}, fmt.Errorf("centralstore: register insert: %w", err)
		}
		current, err = sessionFromDB(raw)
		if err != nil {
			return Session{}, MutationResult{}, err
		}
		rowChanged = true
	} else {
		current = before
		dismissed = before.DismissedAt != nil
		if reg.NewGeneration {
			// A replacement generation must never inherit the previous
			// generation's generation-scoped facts: exit code (ExitedAt is
			// required above for dead replacements), turn state, error state,
			// the status-reported provenance marker, and start time all
			// describe the dead process, not the new one. StatusReported
			// resets WITH working/error (delta review Δ-1): the bit is those
			// facts' provenance marker, and production re-registration
			// replaces Status wholesale from the new runner's /meta — nil
			// until the new process reports (discovery.go:290) — so a
			// resumed generation that dies before reporting must render
			// "status": null and wait-verdict "died", not inherit the dead
			// generation's report. Unread is user-facing attention state and
			// is deliberately kept. Facts observed for the new generation
			// are merged on top below; activity transitions are computed
			// against this reset baseline.
			current.ExitCode = nil
			current.Working = false
			current.Error = false
			current.StatusReported = false
			current.StartedAt = nil
		}
		genBaseline := current
		if err = mergeRunnerFacts(&current, reg.Facts); err != nil {
			return Session{}, MutationResult{}, err
		}
		if reg.Alive {
			current.ExitedAt, current.ExitCode = nil, nil
		}
		current.LastActivityAt = initialActivity(current.LastActivityAt, reg, true, genBaseline)
		current.DismissedAt = nil
		if err = validateMergedSession(current); err != nil {
			return Session{}, MutationResult{}, err
		}
		current.Title, before.Title = "", ""
		rowChanged = !reflect.DeepEqual(current, before)
		if rowChanged {
			cmd, rem, marshalErr := marshalWhole(current.Command, current.Remotes)
			if marshalErr != nil {
				return Session{}, MutationResult{}, marshalErr
			}
			n, updateErr := q.UpdateRunnerRegistration(ctx, db.UpdateRunnerRegistrationParams{
				ConversationRef: nullString(current.ConversationRef), CommandJson: cmd, Cwd: current.CWD, WorkspaceRoot: nullString(current.WorkspaceRoot), RemotesJson: rem,
				Slug: nullString(current.Slug), ShellTitle: nullString(current.ShellTitle), AdapterTitle: nullString(current.AdapterTitle), Subtitle: nullString(current.Subtitle),
				Working: boolInt(current.Working), Unread: boolInt(current.Unread), HasError: boolInt(current.Error), StatusReported: boolInt(current.StatusReported), StartedAtMs: nullMillis(current.StartedAt), ExitedAtMs: nullMillis(current.ExitedAt),
				LastActivityAtMs: nullMillis(current.LastActivityAt), ExitCode: nullInt(current.ExitCode), TerminalCols: nullUint(current.TerminalCols), TerminalRows: nullUint(current.TerminalRows),
				ID: string(reg.ID), RowVersion: int64(before.Version),
			})
			if updateErr != nil {
				return Session{}, MutationResult{}, updateErr
			}
			if n != 1 {
				return Session{}, MutationResult{}, ErrStaleVersion
			}
			current.Version++
		}
	}

	// Takeover evictions run before placement rematching so scope
	// normalization sees the post-eviction state. Each is conditional on the
	// version the coordinator observed; skips are silent by design.
	evicted := false
	for _, ev := range reg.Evict {
		applied, evictErr := evictSessionInTx(ctx, q, ev)
		if evictErr != nil {
			return Session{}, MutationResult{}, evictErr
		}
		evicted = evicted || applied
		// The winner may itself be a direct child of an evicted loser (a live
		// session taking over the conversation of the dead session that
		// launched it). ClearDirectChildParents just nulled the winner's
		// launch parent and bumped its row_version; mirror both on the
		// returned session so it matches the committed row — otherwise the
		// coordinator would install a stale registry version token and the
		// generation's first observation would burn a stale-retry round trip.
		if applied && current.LaunchParentID != nil && *current.LaunchParentID == ev.ID {
			current.LaunchParentID = nil
			current.Version++
		}
	}

	placementChanged, err := rematchRegistration(ctx, q, s.beforePlacementFinalize, current, before, isNew, dismissed)
	if err != nil {
		return Session{}, MutationResult{}, err
	}
	// Belt-and-suspenders: normalizePlacements inside rematchRegistration
	// already detects the loser's cascade-deleted placement, but WorldDirty
	// must not silently depend on that internal — an eviction always deleted
	// a session row and (when placed) its placement, so both payloads are
	// recomposed regardless.
	placementChanged = placementChanged || evicted
	if err = tx.Commit(); err != nil {
		return Session{}, MutationResult{}, err
	}
	current.Title = deriveTitle(current)
	changed := rowChanged || placementChanged || evicted
	return current, MutationResult{
		Changed: changed, SessionVersion: current.Version,
		SessionsDirty: changed, WorldDirty: placementChanged,
	}, nil
}

// evictSessionInTx applies one conditional takeover eviction inside the
// registration transaction. It mirrors RemoveSessionAtVersion's consequences
// (direct children become genuine roots via a cleared launch parent; the
// FK cascade removes placement; the caller renormalizes scopes afterwards)
// but skips instead of failing when the loser vanished or its version moved:
// takeover must never abort the winner's registration.
func evictSessionInTx(ctx context.Context, q *db.Queries, ev TakeoverEviction) (bool, error) {
	ver, err := q.SessionVersion(ctx, string(ev.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // loser already gone
	}
	if err != nil {
		return false, err
	}
	if RowVersion(ver) != ev.Version {
		return false, nil // loser changed since the coordinator's decision
	}
	if _, err = q.ClearDirectChildParents(ctx, nullString(string(ev.ID))); err != nil {
		return false, err
	}
	n, err := q.DeleteSessionAtVersion(ctx, db.DeleteSessionAtVersionParams{ID: string(ev.ID), RowVersion: ver})
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func cloneSessionID(v *SessionID) *SessionID {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}

func validateRunnerFacts(f RunnerFacts) error {
	for name, p := range map[string]NullablePatch[UnixMillis]{"started timestamp": f.StartedAt, "exited timestamp": f.ExitedAt} {
		if p.Set != nil && p.Clear {
			return errors.New("centralstore: nullable patch cannot set and clear")
		}
		if err := validateMillis(name, p.Set); err != nil {
			return err
		}
	}
	if f.ExitCode.Set != nil && f.ExitCode.Clear {
		return errors.New("centralstore: nullable patch cannot set and clear")
	}
	if f.TerminalSize.Set != nil && f.TerminalSize.Clear {
		return errors.New("centralstore: terminal patch cannot set and clear")
	}
	if f.TerminalSize.Set != nil && (f.TerminalSize.Set.Cols == 0 || f.TerminalSize.Set.Rows == 0) {
		return errors.New("centralstore: terminal dimensions must be positive")
	}
	return nil
}

func mergeRunnerFacts(v *Session, f RunnerFacts) error {
	if f.ConversationRef != nil {
		v.ConversationRef = *f.ConversationRef
	}
	if f.CWD != nil {
		v.CWD = *f.CWD
	}
	if f.WorkspaceRoot != nil {
		v.WorkspaceRoot = *f.WorkspaceRoot
	}
	if f.Slug != nil {
		v.Slug = *f.Slug
	}
	if f.ShellTitle != nil {
		v.ShellTitle = *f.ShellTitle
	}
	if f.AdapterTitle != nil {
		v.AdapterTitle = *f.AdapterTitle
	}
	if f.Subtitle != nil {
		v.Subtitle = *f.Subtitle
	}
	if f.Command != nil {
		v.Command = append([]string(nil), (*f.Command)...)
		if v.Command == nil {
			v.Command = []string{}
		}
	}
	if f.Remotes != nil {
		v.Remotes = make(map[string]string, len(*f.Remotes))
		for k, value := range *f.Remotes {
			v.Remotes[k] = value
		}
		if v.Remotes == nil {
			v.Remotes = map[string]string{}
		}
	}
	if f.Working != nil {
		v.Working = *f.Working
	}
	if f.Unread != nil {
		v.Unread = *f.Unread
	}
	if f.Error != nil {
		v.Error = *f.Error
	}
	// Status-reported fact (runner-authoritative): observing a
	// working/error fact proves a status was reported. Sticky WITHIN a
	// generation (the daemon's SSE path flattens an in-generation null
	// status to {working:false}, subscribe.go:219–226); the generation
	// boundary resets it in RegisterRunner's NewGeneration block, because
	// production re-registration replaces Status wholesale from the new
	// runner's /meta — nil until the new process reports (discovery.go:290).
	v.StatusReported = v.StatusReported || f.Working != nil || f.Error != nil
	if err := applyNullable(&v.StartedAt, f.StartedAt); err != nil {
		return err
	}
	if err := applyNullable(&v.ExitedAt, f.ExitedAt); err != nil {
		return err
	}
	if err := applyNullable(&v.ExitCode, f.ExitCode); err != nil {
		return err
	}
	if f.TerminalSize.Clear {
		v.TerminalCols, v.TerminalRows = nil, nil
	}
	if f.TerminalSize.Set != nil {
		cols, rows := f.TerminalSize.Set.Cols, f.TerminalSize.Set.Rows
		v.TerminalCols, v.TerminalRows = &cols, &rows
	}
	return nil
}

func validateMergedSession(v Session) error {
	if v.Adapter == "" {
		return errors.New("centralstore: adapter required")
	}
	if v.Command == nil || v.Remotes == nil {
		return errors.New("centralstore: command and remotes required")
	}
	for name, x := range map[string]*UnixMillis{"started timestamp": v.StartedAt, "exited timestamp": v.ExitedAt, "activity timestamp": v.LastActivityAt} {
		if err := validateMillis(name, x); err != nil {
			return err
		}
	}
	return validateTerminal(v.TerminalCols, v.TerminalRows)
}

func initialActivity(existing *UnixMillis, reg RunnerRegistration, hadRow bool, before Session) *UnixMillis {
	var bump bool
	if hadRow {
		bump = (!before.Working && valueBool(reg.Facts.Working, before.Working)) ||
			(!before.Unread && valueBool(reg.Facts.Unread, before.Unread)) ||
			(!before.Error && valueBool(reg.Facts.Error, before.Error)) ||
			(reg.NewGeneration && !reg.Alive)
	} else {
		// A brand-new row collapses transitions that previously landed on an
		// existing row into one insert; the activity bump those transitions
		// would have produced must not be lost. A fast-dead insert counts as
		// a death transition.
		bump = valueBool(reg.Facts.Working, false) ||
			valueBool(reg.Facts.Unread, false) ||
			valueBool(reg.Facts.Error, false) ||
			!reg.Alive
	}
	if !bump {
		return existing
	}
	if existing != nil && *existing >= reg.ObservedAt {
		return existing
	}
	x := reg.ObservedAt
	return &x
}

func valueBool(observed *bool, fallback bool) bool {
	if observed == nil {
		return fallback
	}
	return *observed
}

// matchCatalog is the single project-attribution entry point shared by the
// registration-time rematch and the catalog-replacement rematch
// (ReplaceProjectCatalogAndRematch): both derive membership from the same
// projectmatch policy over the same catalog DTO, so they provably agree.
func matchCatalog(catalog ProjectCatalog, in projectmatch.Inputs) (ProjectEntryID, bool) {
	entries := make([]projectmatch.Entry, len(catalog))
	for i, entry := range catalog {
		entries[i].Reference = entry.Kind == ProjectEntryReference
		entries[i].Rules = make([]projectmatch.Rule, len(entry.Rules))
		for j, rule := range entry.Rules {
			entries[i].Rules[j] = projectmatch.Rule{Path: rule.Path, Remote: rule.Remote, Exact: rule.Exact}
		}
	}
	index, ok := projectmatch.Match(entries, in)
	if !ok {
		return 0, false
	}
	return catalog[index].ID, true
}

func rematchRegistration(ctx context.Context, q *db.Queries, fault func() error, current, before Session, isNew, dismissed bool) (bool, error) {
	placementChanged := false
	all, err := placements(ctx, q)
	if err != nil {
		return false, err
	}
	var target *placementRec
	for _, r := range all {
		if r.local == string(current.ID) {
			target = r
			break
		}
	}
	inputsChanged := !isNew && (current.CWD != before.CWD || current.WorkspaceRoot != before.WorkspaceRoot || !reflect.DeepEqual(current.Remotes, before.Remotes))
	if !isNew && !dismissed && target != nil && !inputsChanged {
		// Even when this session does not move, insertion of a previously
		// missing parent can regroup already-placed children.
		return normalizePlacements(ctx, q, fault)
	}

	catalog, err := catalogFromQueries(ctx, q)
	if err != nil {
		return false, err
	}
	project64, matched := matchCatalog(catalog, projectmatch.Inputs{CWD: current.CWD, WorkspaceRoot: current.WorkspaceRoot, Remotes: current.Remotes})

	if target != nil && (!matched || dismissed) {
		n, deleteErr := q.DeleteLocalSessionPlacement(ctx, nullString(string(current.ID)))
		if deleteErr != nil {
			return false, deleteErr
		}
		if n != 1 {
			return false, errors.New("centralstore: placement disappeared during registration")
		}
		kept := all[:0]
		for _, r := range all {
			if r != target {
				kept = append(kept, r)
			}
		}
		all = kept
		target = nil
		placementChanged = true
	}
	if dismissed {
		// A reappearing row intentionally loses its old order, but still uses
		// the ordinary matcher below and appends in its new scope.
		if matched { /* continue */
		} else {
			normalized, normalizeErr := normalizePlacements(ctx, q, fault)
			return placementChanged || normalized, normalizeErr
		}
	}
	if !matched {
		normalized, normalizeErr := normalizePlacements(ctx, q, fault)
		return placementChanged || normalized, normalizeErr
	}
	project := int64(project64)
	if target == nil {
		target = &placementRec{project: project, local: string(current.ID), parent: func() string {
			if current.LaunchParentID == nil {
				return ""
			}
			return string(*current.LaunchParentID)
		}(), created: int64(current.CreatedAt), promoted: current.PromotedToRoot, isNew: true}
		all = append(all, target)
	} else {
		target.project = project
	}
	rewritten, rewriteErr := rewritePlacements(ctx, q, all, nil, fault)
	return placementChanged || rewritten, rewriteErr
}
