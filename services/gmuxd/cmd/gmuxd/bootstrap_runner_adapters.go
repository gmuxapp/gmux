package main

// Production runner transport and spawn policy for the central coordinator.
// These adapters are deliberately inert until selected by the S5 composition.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/packages/sessionenv"
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
		if line == "" {
			typ = ""
			continue
		}
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
	var s runnerMetaWire
	if err = json.Unmarshal(body, &s); err != nil {
		return sessioncoord.RunnerMeta{}, err
	}
	if s.Adapter == "" {
		s.Adapter = s.Kind
	}
	if s.ID == "" || s.Adapter == "" {
		return sessioncoord.RunnerMeta{}, fmt.Errorf("runner /meta: missing id or adapter")
	}
	reg := centralstore.RunnerRegistration{ID: centralstore.SessionID(s.ID), Adapter: s.Adapter, Alive: s.Alive, CreatedAt: parseMillis(s.CreatedAt), ObservedAt: centralstore.UnixMillis(time.Now().UnixMilli())}
	reg.Facts = runnerMetaFacts(s)
	return sessioncoord.RunnerMeta{Registration: reg, PID: s.PID, RunnerVersion: s.RunnerVersion, BinaryHash: s.BinaryHash}, nil
}
func parseMillis(v string) centralstore.UnixMillis {
	t, _ := time.Parse(time.RFC3339, v)
	return centralstore.UnixMillis(t.UnixMilli())
}

type runnerMetaWire struct {
	ID, Adapter, Kind string
	Alive             bool              `json:"alive"`
	CreatedAt         string            `json:"created_at"`
	PID               int               `json:"pid"`
	RunnerVersion     string            `json:"runner_version"`
	BinaryHash        string            `json:"binary_hash"`
	ConversationRef   string            `json:"conversation_file"`
	CWD               string            `json:"cwd"`
	WorkspaceRoot     string            `json:"workspace_root"`
	Slug              string            `json:"slug"`
	ShellTitle        string            `json:"shell_title"`
	AdapterTitle      string            `json:"adapter_title"`
	Subtitle          string            `json:"subtitle"`
	Command           []string          `json:"command"`
	Remotes           map[string]string `json:"remotes"`
	Status            *struct {
		Working bool `json:"working"`
		Error   bool `json:"error"`
	} `json:"status"`
	Unread       bool   `json:"unread"`
	TerminalCols uint16 `json:"terminal_cols"`
	TerminalRows uint16 `json:"terminal_rows"`
}

func runnerMetaFacts(s runnerMetaWire) centralstore.RunnerFacts {
	f := centralstore.RunnerFacts{ConversationRef: &s.ConversationRef, CWD: &s.CWD, WorkspaceRoot: &s.WorkspaceRoot, Slug: &s.Slug, ShellTitle: &s.ShellTitle, AdapterTitle: &s.AdapterTitle, Subtitle: &s.Subtitle, Command: &s.Command, Remotes: &s.Remotes}
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

type runnerLaunchRequest struct {
	GmuxBin           string
	Command           []string
	CWD, ResumeID     string
	InitialCols, Rows uint16
	Endpoint          string
}

type runnerLaunchResult struct {
	Endpoint  string
	PID       int
	Wait      <-chan error
	Terminate func(context.Context) error
}

type productionRunnerSpawner struct {
	Projects       *projects.Manager
	GmuxBin        string
	ResolveDir     func(centralstore.Session) (string, error)
	ResolveCommand func(centralstore.Session) []string
	Launch         func(context.Context, runnerLaunchRequest) (runnerLaunchResult, error)
	mu             sync.Mutex
	launched       map[string]runnerLaunchResult
}

func (s *productionRunnerSpawner) Spawn(ctx context.Context, row centralstore.Session) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	legacy := store.Session{ID: string(row.ID), Adapter: row.Adapter, ConversationRef: row.ConversationRef, Cwd: row.CWD, WorkspaceRoot: row.WorkspaceRoot, Remotes: row.Remotes, Command: append([]string(nil), row.Command...)}
	var cwd string
	var err error
	if s.ResolveDir != nil {
		cwd, err = s.ResolveDir(row)
	} else {
		if s.Projects == nil {
			return "", fmt.Errorf("runner spawn: projects unavailable")
		}
		cwd, _ = resolveResumeDir(s.Projects, legacy)
	}
	if err != nil {
		return "", err
	}
	if cwd == "" {
		return "", fmt.Errorf("runner spawn: no usable directory")
	}
	if s.ResolveCommand != nil {
		legacy.Command = s.ResolveCommand(row)
	} else {
		legacy.Command = discovery.ResolveResumeCommand(&legacy)
	}
	if len(legacy.Command) == 0 {
		return "", fmt.Errorf("runner spawn: session %s is not resumable", row.ID)
	}
	endpoint := filepath.Join(paths.SessionSocketDir(), legacy.ID+".sock")
	launch := s.Launch
	if launch == nil {
		launch = launchRunnerProcess
	}
	result, err := launch(ctx, runnerLaunchRequest{GmuxBin: s.GmuxBin, Command: legacy.Command, CWD: cwd, ResumeID: legacy.ID, InitialCols: value16(row.TerminalCols), Rows: value16(row.TerminalRows), Endpoint: endpoint})
	if err != nil {
		return "", err
	}
	if result.Endpoint == "" {
		result.Endpoint = endpoint
	}
	if result.Terminate == nil {
		return "", fmt.Errorf("runner spawn: launch result has no termination handle")
	}
	s.mu.Lock()
	if s.launched == nil {
		s.launched = make(map[string]runnerLaunchResult)
	}
	s.launched[result.Endpoint] = result
	s.mu.Unlock()
	return result.Endpoint, nil
}

func (s *productionRunnerSpawner) CleanupSpawn(ctx context.Context, endpoint string) error {
	s.mu.Lock()
	result, ok := s.launched[endpoint]
	delete(s.launched, endpoint)
	s.mu.Unlock()
	if !ok || result.Terminate == nil {
		return nil
	}
	return result.Terminate(ctx)
}

// FinalizeSpawn transfers process ownership to the registered runtime and
// drops launch closures without signalling the child.
func (s *productionRunnerSpawner) FinalizeSpawn(endpoint string) {
	s.mu.Lock()
	delete(s.launched, endpoint)
	s.mu.Unlock()
}
func launchRunnerProcess(ctx context.Context, req runnerLaunchRequest) (runnerLaunchResult, error) {
	if err := ctx.Err(); err != nil {
		return runnerLaunchResult{}, err
	}
	cmd := exec.Command(req.GmuxBin, buildLaunchArgs(req.ResumeID, req.InitialCols, req.Rows, req.Command)...)
	cmd.Dir = req.CWD
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = sessionenv.Strip(captureLoginEnv(req.GmuxBin, req.CWD))
	if err := cmd.Start(); err != nil {
		if req.CWD != "" && !projects.IsDir(req.CWD) {
			return runnerLaunchResult{}, fmt.Errorf("working directory %q does not exist: %w", req.CWD, err)
		}
		return runnerLaunchResult{}, err
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait(); close(wait) }()
	terminate := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cmd.Process == nil {
			return nil
		}
		err := cmd.Process.Signal(syscall.SIGTERM)
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		select {
		case <-wait:
			return nil
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return ctx.Err()
		}
	}
	return runnerLaunchResult{Endpoint: req.Endpoint, PID: cmd.Process.Pid, Wait: wait, Terminate: terminate}, nil
}

func value16(v *uint16) uint16 {
	if v == nil {
		return 0
	}
	return *v
}

var _ sessioncoord.RunnerClient = productionRunnerClient{}
var _ sessioncoord.RunnerSpawner = (*productionRunnerSpawner)(nil)
var _ sessioncoord.RunnerSpawnCleaner = (*productionRunnerSpawner)(nil)
