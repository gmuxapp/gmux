package main

// Production runner transport and spawn policy for the central coordinator.
// These adapters are deliberately inert until selected by the S5 composition.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/projects"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

type productionRunnerClient struct{}

type productionEventStream struct {
	events chan sessioncoord.RunnerEvent
	cancel context.CancelFunc
	body   io.ReadCloser
	once   sync.Once
}

func (s *productionEventStream) Events() <-chan sessioncoord.RunnerEvent { return s.events }
func (s *productionEventStream) Close() error {
	var err error
	s.once.Do(func() { s.cancel(); err = s.body.Close() })
	return err
}

func runnerHTTPClient(endpoint string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
	}}}
}
func runnerRequestContext(ctx context.Context, endpoint, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://runner"+path, nil)
	if err != nil {
		return nil, err
	}
	return runnerHTTPClient(endpoint).Do(req)
}

// Subscribe returns only after HTTP headers establish /events. The reader is
// started immediately, so events emitted before the subsequent /meta request
// are retained in the bounded channel.
func (productionRunnerClient) Subscribe(ctx context.Context, endpoint string) (sessioncoord.EventStream, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	resp, err := runnerRequestContext(streamCtx, endpoint, http.MethodGet, "/events")
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("runner /events: %s", resp.Status)
	}
	s := &productionEventStream{events: make(chan sessioncoord.RunnerEvent, 64), cancel: cancel, body: resp.Body}
	go func() { defer close(s.events); defer s.Close(); scanRunnerEvents(streamCtx, resp.Body, s.events) }()
	return s, nil
}

func scanRunnerEvents(ctx context.Context, r io.Reader, out chan<- sessioncoord.RunnerEvent) {
	sc := bufio.NewScanner(r)
	var typ string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			typ = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") || typ == "" {
			continue
		}
		raw := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		ev, ok := runnerEventProjection(typ, raw)
		typ = ""
		if !ok {
			continue
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return
		}
	}
}
func runnerEventProjection(typ string, raw []byte) (sessioncoord.RunnerEvent, bool) {
	now := centralstore.UnixMillis(time.Now().UnixMilli())
	f := centralstore.RunnerFacts{}
	switch typ {
	case "status":
		var v struct {
			Working bool `json:"working"`
			Error   bool `json:"error"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return sessioncoord.RunnerEvent{}, false
		}
		f.Working = &v.Working
		f.Error = &v.Error
	case "meta":
		var v struct {
			ShellTitle, AdapterTitle, Subtitle, Slug *string
			Unread                                   *bool `json:"unread"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return sessioncoord.RunnerEvent{}, false
		}
		f.ShellTitle = v.ShellTitle
		f.AdapterTitle = v.AdapterTitle
		f.Subtitle = v.Subtitle
		f.Slug = v.Slug
		f.Unread = v.Unread
	case "conversation_file", "session_file":
		var v struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(raw, &v) != nil || v.Path == "" {
			return sessioncoord.RunnerEvent{}, false
		}
		f.ConversationRef = &v.Path
	case "terminal_resize":
		var v centralstore.TerminalSize
		if json.Unmarshal(raw, &v) != nil {
			return sessioncoord.RunnerEvent{}, false
		}
		f.TerminalSize = centralstore.NullablePatch[centralstore.TerminalSize]{Set: &v}
	case "exit":
		var v struct {
			ExitCode int `json:"exit_code"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return sessioncoord.RunnerEvent{}, false
		}
		alive := false
		f.ExitCode = centralstore.NullablePatch[int]{Set: &v.ExitCode}
		f.ExitedAt = centralstore.NullablePatch[centralstore.UnixMillis]{Set: &now}
		return sessioncoord.RunnerEvent{ObservedAt: now, Facts: f, Alive: &alive}, true
	case "activity":
		return sessioncoord.RunnerEvent{ObservedAt: now, TransientActivity: true}, true
	default:
		return sessioncoord.RunnerEvent{}, false
	}
	return sessioncoord.RunnerEvent{ObservedAt: now, Facts: f}, true
}

func (productionRunnerClient) Meta(ctx context.Context, endpoint string) (sessioncoord.RunnerMeta, error) {
	resp, err := runnerRequestContext(ctx, endpoint, http.MethodGet, "/meta")
	if err != nil {
		return sessioncoord.RunnerMeta{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return sessioncoord.RunnerMeta{}, fmt.Errorf("runner /meta: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return sessioncoord.RunnerMeta{}, err
	}
	var s store.Session
	if err = json.Unmarshal(body, &s); err != nil {
		return sessioncoord.RunnerMeta{}, err
	}
	if s.Adapter == "" {
		var l struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(body, &l)
		s.Adapter = l.Kind
	}
	if s.ID == "" || s.Adapter == "" {
		return sessioncoord.RunnerMeta{}, fmt.Errorf("runner /meta: missing id or adapter")
	}
	reg := centralstore.RunnerRegistration{ID: centralstore.SessionID(s.ID), Adapter: s.Adapter, Alive: s.Alive, CreatedAt: parseMillis(s.CreatedAt), ObservedAt: centralstore.UnixMillis(time.Now().UnixMilli())}
	reg.Facts = storeFacts(s)
	return sessioncoord.RunnerMeta{Registration: reg, PID: s.Pid, RunnerVersion: s.RunnerVersion, BinaryHash: s.BinaryHash}, nil
}
func parseMillis(v string) centralstore.UnixMillis {
	t, _ := time.Parse(time.RFC3339, v)
	return centralstore.UnixMillis(t.UnixMilli())
}
func storeFacts(s store.Session) centralstore.RunnerFacts {
	f := centralstore.RunnerFacts{ConversationRef: &s.ConversationRef, CWD: &s.Cwd, WorkspaceRoot: &s.WorkspaceRoot, Slug: &s.Slug, ShellTitle: &s.ShellTitle, AdapterTitle: &s.AdapterTitle, Subtitle: &s.Subtitle, Command: &s.Command, Remotes: &s.Remotes}
	if s.Status != nil {
		f.Working = &s.Status.Working
		f.Error = &s.Status.Error
	}
	f.Unread = &s.Unread
	if s.TerminalCols > 0 && s.TerminalRows > 0 {
		x := centralstore.TerminalSize{Cols: s.TerminalCols, Rows: s.TerminalRows}
		f.TerminalSize.Set = &x
	}
	return f
}

type productionRunnerSpawner struct {
	Projects *projects.Manager
	GmuxBin  string
	Launch   func(string, []string, string, string, uint16, uint16) (int, error)
}

func (s productionRunnerSpawner) Spawn(ctx context.Context, row centralstore.Session) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	legacy := store.Session{ID: string(row.ID), Cwd: row.CWD, WorkspaceRoot: row.WorkspaceRoot, Remotes: row.Remotes, Command: row.Command, ProjectSlug: ""}
	if s.Projects == nil {
		return "", fmt.Errorf("runner spawn: projects unavailable")
	}
	cwd, _ := resolveResumeDir(s.Projects, legacy)
	if cwd == "" {
		return "", fmt.Errorf("runner spawn: no usable directory")
	}
	discovery.ResolveResumeCommand(&legacy)
	launch := s.Launch
	if launch == nil {
		launch = launchGmux
	}
	if _, err := launch(s.GmuxBin, legacy.Command, cwd, legacy.ID, value16(row.TerminalCols), value16(row.TerminalRows)); err != nil {
		return "", err
	}
	return filepath.Join(paths.SessionSocketDir(), legacy.ID+".sock"), nil
}
func value16(v *uint16) uint16 {
	if v == nil {
		return 0
	}
	return *v
}

var _ sessioncoord.RunnerClient = productionRunnerClient{}
var _ sessioncoord.RunnerSpawner = productionRunnerSpawner{}
var _ = os.ErrNotExist
