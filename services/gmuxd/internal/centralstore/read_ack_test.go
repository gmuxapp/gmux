package centralstore

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestAcknowledgeDeadSessionClearsUnreadAndErrorOnly(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	started := UnixMillis(2)
	exited := UnixMillis(3)
	cols, rows := uint16(100), uint16(40)
	before, _, err := s.InsertSession(ctx, NewSession{
		ID: "dead", Adapter: "shell", ConversationRef: "conversation",
		Command: []string{"sh", "-l"}, CWD: "/work", WorkspaceRoot: "/work",
		Remotes: map[string]string{"origin": "git@example/repo"}, Slug: "slug",
		ShellTitle: "title", Subtitle: "subtitle", Working: true, Unread: true,
		Error: true, CreatedAt: 1, StartedAt: &started, ExitedAt: &exited,
		TerminalCols: &cols, TerminalRows: &rows,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := s.AcknowledgeDeadSession(ctx, before.ID, before.Version)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.SessionVersion != before.Version+1 || !result.SessionsDirty || result.WorldDirty {
		t.Fatalf("result = %#v", result)
	}
	got, ok, err := s.Session(ctx, before.ID)
	if err != nil || !ok {
		t.Fatalf("session ok=%v err=%v", ok, err)
	}
	if got.Unread || got.Error {
		t.Fatalf("acknowledgement not applied: %#v", got)
	}
	before.Unread, before.Error, before.Version = false, false, before.Version+1
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("unrelated facts changed:\n got  %#v\n want %#v", got, before)
	}
}

func TestAcknowledgeDeadSessionSingleIndicatorAndNoop(t *testing.T) {
	for _, tc := range []struct {
		name           string
		unread, failed bool
		wantChanged    bool
	}{
		{name: "unread only", unread: true, wantChanged: true},
		{name: "error only", failed: true, wantChanged: true},
		{name: "already acknowledged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openKernelStore(t)
			row, _, err := s.InsertSession(ctx, NewSession{ID: "dead", Adapter: "shell", Command: []string{}, CWD: "/", Remotes: map[string]string{}, Unread: tc.unread, Error: tc.failed, CreatedAt: 1})
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.AcknowledgeDeadSession(ctx, row.ID, row.Version)
			if err != nil {
				t.Fatal(err)
			}
			if result.Changed != tc.wantChanged || result.SessionsDirty != tc.wantChanged || result.WorldDirty {
				t.Fatalf("result = %#v", result)
			}
			wantVersion := row.Version
			if tc.wantChanged {
				wantVersion++
			}
			if result.SessionVersion != wantVersion {
				t.Fatalf("version = %d, want %d", result.SessionVersion, wantVersion)
			}
		})
	}
}

func TestAcknowledgeDeadSessionRejectsStaleAndMissingRows(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	row, _, err := s.InsertSession(ctx, NewSession{ID: "dead", Adapter: "shell", Command: []string{}, CWD: "/", Remotes: map[string]string{}, Unread: true, Error: true, CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	working := true
	advanced, err := s.ApplyCommonFacts(ctx, row.ID, row.Version, CommonFactsPatch{Working: &working})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AcknowledgeDeadSession(ctx, row.ID, row.Version)
	if !errors.Is(err, ErrStaleVersion) || result.SessionVersion != advanced.SessionVersion || result.Changed || result.SessionsDirty || result.WorldDirty {
		t.Fatalf("stale result=%#v err=%v", result, err)
	}
	got, _, err := s.Session(ctx, row.ID)
	if err != nil || !got.Unread || !got.Error {
		t.Fatalf("stale acknowledgement changed row: %#v err=%v", got, err)
	}
	if _, err = s.AcknowledgeDeadSession(ctx, "missing", 1); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing error = %v, want ErrSessionNotFound", err)
	}
}

type readAckCoordinator struct {
	mu   sync.Mutex
	live map[SessionID]bool
}

func (c *readAckCoordinator) acknowledge(ctx context.Context, s *Store, id SessionID, observed RowVersion) (MutationResult, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live[id] {
		return MutationResult{}, false, nil
	}
	result, err := s.AcknowledgeDeadSession(ctx, id, observed)
	return result, true, err
}

func TestDeadSessionAcknowledgementDurableTracer(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	row, _, err := s.InsertSession(ctx, NewSession{ID: "dead", Adapter: "shell", Command: []string{"sh"}, CWD: "/work", Remotes: map[string]string{}, Working: true, Unread: true, Error: true, CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &readAckCoordinator{live: map[SessionID]bool{}}

	coordinator.live[row.ID] = true
	if result, attempted, err := coordinator.acknowledge(ctx, s, row.ID, row.Version); err != nil || attempted || result != (MutationResult{}) {
		t.Fatalf("live acknowledgement result=%#v attempted=%v err=%v", result, attempted, err)
	}
	coordinator.live[row.ID] = false

	result, attempted, err := coordinator.acknowledge(ctx, s, row.ID, row.Version)
	if err != nil || !attempted || !result.Changed || !result.SessionsDirty {
		t.Fatalf("dead acknowledgement result=%#v attempted=%v err=%v", result, attempted, err)
	}
	snapshot, err := s.ListSessions(ctx)
	if err != nil || len(snapshot) != 1 || snapshot[0].Unread || snapshot[0].Error || !snapshot[0].Working {
		t.Fatalf("committed snapshot=%#v err=%v", snapshot, err)
	}

	// Acknowledgement and registration share the coordinator lock. Here
	// acknowledgement won, so registration consumes its committed version.
	coordinator.mu.Lock()
	coordinator.live[row.ID] = true
	newUnread := true
	registered, regErr := s.ApplyCommonFacts(ctx, row.ID, result.SessionVersion, CommonFactsPatch{Unread: &newUnread})
	coordinator.mu.Unlock()
	if regErr != nil || registered.SessionVersion != result.SessionVersion+1 {
		t.Fatalf("registration result=%#v err=%v", registered, regErr)
	}
	// Registration-first establishes liveness and prevents acknowledgement.
	if _, attempted, err = coordinator.acknowledge(ctx, s, row.ID, result.SessionVersion); err != nil || attempted {
		t.Fatalf("registration-first acknowledgement attempted=%v err=%v", attempted, err)
	}

	coordinator.mu.Lock()
	coordinator.live[row.ID] = false
	coordinator.mu.Unlock()
	result, attempted, err = coordinator.acknowledge(ctx, s, row.ID, registered.SessionVersion)
	if err != nil || !attempted || !result.Changed {
		t.Fatalf("final acknowledgement result=%#v attempted=%v err=%v", result, attempted, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reopened, err := s.ListSessions(ctx)
	if err != nil || len(reopened) != 1 || reopened[0].Unread || reopened[0].Error || reopened[0].Version != result.SessionVersion {
		t.Fatalf("reopened snapshot=%#v result=%#v err=%v", reopened, result, err)
	}
}
