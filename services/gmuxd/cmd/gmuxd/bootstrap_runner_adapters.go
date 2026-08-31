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
	"github.com/gmuxapp/gmux/packages/socklease"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

// runnerIncarnationHeader must match ptyserver.IncarnationHeader. It is
// duplicated rather than imported because the runner and the daemon are
// separate modules with no shared HTTP surface; the value is part of the
// runner protocol and is covered by a contract test.
const runnerIncarnationHeader = "X-Gmux-Incarnation"

type productionRunnerClient struct{}

type productionEventStream struct {
	events      chan sessioncoord.RunnerEvent
	cancel      context.CancelFunc
	body        io.ReadCloser
	incarnation string
	once        sync.Once
}

func (s *productionEventStream) Events() <-chan sessioncoord.RunnerEvent { return s.events }

// Incarnation reports which runner served this subscription, from the header
// the runner stamps on every response. It implements
// sessioncoord.StreamIncarnation; an empty value means the runner predates the
// protocol.
func (s *productionEventStream) Incarnation() string { return s.incarnation }
func (s *productionEventStream) Close() error {
	var err error
	s.once.Do(func() { s.cancel(); err = s.body.Close() })
	return err
}

func runnerHTTPClient(endpoint string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		// Each client owns exactly one request or stream and is then discarded.
		// Never leave its connection in an unreachable idle pool.
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
		},
	}}
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
	s := &productionEventStream{
		events:      make(chan sessioncoord.RunnerEvent, 64),
		cancel:      cancel,
		body:        resp.Body,
		incarnation: resp.Header.Get(runnerIncarnationHeader),
	}
	go func() { defer close(s.events); defer s.Close(); scanRunnerEvents(streamCtx, resp.Body, s.events) }()
	return s, nil
}

// maxRunnerEventLine bounds one SSE data line from a runner. It is sized for
// the worst-case ESCAPED turn frame: the adapter caps a turn's output at 256 KiB
// pre-escape and JSON escaping can expand a byte six-fold, so a smaller limit
// would make a large answer kill the stream (bufio's default is 64 KiB, which a
// perfectly ordinary answer can exceed). Sized jointly with the runner's
// hook-body limit; see docs/runner-hook-protocol.md.
const maxRunnerEventLine = 8 << 20

func scanRunnerEvents(ctx context.Context, r io.Reader, out chan<- sessioncoord.RunnerEvent) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxRunnerEventLine)
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
		// A turn edge carries the turn frame inside its status event, so the
		// close and the result it asserted cannot be separated in transit (see
		// docs/runner-hook-protocol.md). Absent for a status write that is not a
		// turn edge — a raw PUT /status, a shell session's lifetime turn, or a
		// runner too old to send one — which is exactly the frame-less case that
		// resolves result-free.
		var v struct {
			Active      bool                    `json:"active"`
			Error       bool                    `json:"error"`
			Interrupted bool                    `json:"interrupted"`
			Frame       *sessioncoord.TurnFrame `json:"turn_frame"`
			Unread      *bool                   `json:"unread"`
			UnreadToken *string                 `json:"unread_token"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return sessioncoord.RunnerEvent{}, false
		}
		f.Active = &v.Active
		f.Error = &v.Error
		f.Interrupted = &v.Interrupted
		f.Unread = v.Unread
		f.UnreadToken = v.UnreadToken
		if v.Frame != nil {
			return sessioncoord.RunnerEvent{ObservedAt: now, Facts: f, Frame: v.Frame}, true
		}
	case "meta":
		// Field tags are load-bearing: the runner emits snake_case keys, and
		// Go's case-insensitive fallback does NOT bridge snake_case to camel
		// case — untagged fields here silently dropped every live title
		// update (sessions only got titles from the tagged /meta struct at
		// registration/convergence time).
		var v struct {
			ShellTitle   *string `json:"shell_title"`
			AdapterTitle *string `json:"adapter_title"`
			Subtitle     *string `json:"subtitle"`
			Slug         *string `json:"slug"`
			Unread       *bool   `json:"unread"`
			UnreadToken  *string `json:"unread_token"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return sessioncoord.RunnerEvent{}, false
		}
		f.ShellTitle = v.ShellTitle
		f.AdapterTitle = v.AdapterTitle
		f.Subtitle = v.Subtitle
		f.Slug = v.Slug
		f.Unread = v.Unread
		f.UnreadToken = v.UnreadToken
	case "conversation_file", "session_file":
		var v struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(raw, &v) != nil || v.Path == "" {
			return sessioncoord.RunnerEvent{}, false
		}
		f.ConversationRef = &v.Path
		// Conversation-local status cannot cross a rebind. Clear it without
		// claiming that the new conversation reported idle.
		f.ResetStatus = true
	case "terminal_resize":
		var v centralstore.TerminalSize
		if json.Unmarshal(raw, &v) != nil {
			return sessioncoord.RunnerEvent{}, false
		}
		f.TerminalSize = centralstore.NullablePatch[centralstore.TerminalSize]{Set: &v}
	case "exit":
		var v struct {
			ExitCode    int     `json:"exit_code"`
			ExitedAt    string  `json:"exited_at"`
			Active      *bool   `json:"active"`
			Error       *bool   `json:"error"`
			Interrupted *bool   `json:"interrupted"`
			Unread      *bool   `json:"unread"`
			UnreadToken *string `json:"unread_token"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return sessioncoord.RunnerEvent{}, false
		}
		exitedAt := now
		if v.ExitedAt != "" {
			if t, err := time.Parse(time.RFC3339, v.ExitedAt); err == nil {
				exitedAt = centralstore.UnixMillis(t.UnixMilli())
			}
		}
		alive := false
		f.ExitCode = centralstore.NullablePatch[int]{Set: &v.ExitCode}
		f.ExitedAt = centralstore.NullablePatch[centralstore.UnixMillis]{Set: &exitedAt}
		f.Active = v.Active
		f.Error = v.Error
		f.Interrupted = v.Interrupted
		f.Unread = v.Unread
		f.UnreadToken = v.UnreadToken
		return sessioncoord.RunnerEvent{ObservedAt: now, Facts: f, Alive: &alive}, true
	case "turn_frame":
		// A frame update with no status transition to ride: a mid-turn injection,
		// a rebind clear, or the connect-time replay snapshot. Runtime-only — it
		// carries no durable facts, so it is retained without a store write (see
		// Coordinator.drain).
		var v sessioncoord.TurnFrame
		if json.Unmarshal(raw, &v) != nil {
			return sessioncoord.RunnerEvent{}, false
		}
		return sessioncoord.RunnerEvent{ObservedAt: now, Frame: &v, FrameOnly: true}, true
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
	reg := centralstore.RunnerRegistration{ID: centralstore.SessionID(s.ID), Adapter: s.Adapter, DriveMode: s.DriveMode, Alive: s.Alive, CreatedAt: parseMillis(s.CreatedAt), ObservedAt: centralstore.UnixMillis(time.Now().UnixMilli())}
	if s.ParentSessionID != "" {
		parent := centralstore.SessionID(s.ParentSessionID)
		reg.ParentSessionID = &parent
	}
	reg.Facts = runnerMetaFacts(s)
	// Prefer the header: it is stamped by the same middleware that stamps the
	// subscription response, so the two comparisons cannot disagree because
	// of a body-serialisation quirk. The body field is the fallback for a
	// transport (or proxy) that drops headers.
	incarnation := resp.Header.Get(runnerIncarnationHeader)
	if incarnation == "" {
		incarnation = s.Incarnation
	}
	return sessioncoord.RunnerMeta{Registration: reg, PID: s.PID, RunnerVersion: s.RunnerVersion, BinaryHash: s.BinaryHash, Incarnation: incarnation}, nil
}
func parseMillis(v string) centralstore.UnixMillis {
	t, _ := time.Parse(time.RFC3339, v)
	return centralstore.UnixMillis(t.UnixMilli())
}

type runnerMetaWire struct {
	ID              string            `json:"id"`
	Incarnation     string            `json:"incarnation"`
	Adapter         string            `json:"adapter"`
	DriveMode       string            `json:"drive_mode"` // empty (older runner) = terminal
	Kind            string            `json:"kind"`
	Alive           bool              `json:"alive"`
	CreatedAt       string            `json:"created_at"`
	StartedAt       string            `json:"started_at"`
	ExitCode        *int              `json:"exit_code"`
	ExitedAt        string            `json:"exited_at"`
	PID             int               `json:"pid"`
	RunnerVersion   string            `json:"runner_version"`
	BinaryHash      string            `json:"binary_hash"`
	ConversationRef string            `json:"conversation_file"`
	CWD             string            `json:"cwd"`
	WorkspaceRoot   string            `json:"workspace_root"`
	Slug            string            `json:"slug"`
	ShellTitle      string            `json:"shell_title"`
	AdapterTitle    string            `json:"adapter_title"`
	Subtitle        string            `json:"subtitle"`
	Command         []string          `json:"command"`
	Remotes         map[string]string `json:"remotes"`
	ParentSessionID string            `json:"parent_session_id"`
	Status          *struct {
		Active      bool `json:"active"`
		Error       bool `json:"error"`
		Interrupted bool `json:"interrupted"`
	} `json:"status"`
	Unread       bool   `json:"unread"`
	UnreadToken  string `json:"unread_token"`
	TerminalCols uint16 `json:"terminal_cols"`
	TerminalRows uint16 `json:"terminal_rows"`
}

func runnerMetaFacts(s runnerMetaWire) centralstore.RunnerFacts {
	// Conversation metadata is a last-known-good materialized projection of
	// the adapter-owned conversation. A runner can register before its hook has
	// rebound (the ref and metadata are all empty), so that pre-bind snapshot is
	// unobserved rather than an authoritative clear. Once the ref is non-empty,
	// the whole metadata snapshot is authoritative, including empty clears.
	// A later different conversation_file event is the rebind boundary;
	// centralstore clears the previous conversation's metadata there before
	// applying facts for the new binding.
	f := centralstore.RunnerFacts{CWD: &s.CWD, WorkspaceRoot: &s.WorkspaceRoot, ShellTitle: &s.ShellTitle, Command: &s.Command, Remotes: &s.Remotes}
	if s.ConversationRef != "" {
		// Once the runner is positively bound, its metadata snapshot is
		// authoritative even when a value is empty: a clear that happened while
		// gmuxd was disconnected must converge on registration. Only the
		// pre-bind empty-ref window treats these fields as unobserved.
		f.ConversationRef = &s.ConversationRef
		f.Slug = &s.Slug
		f.AdapterTitle = &s.AdapterTitle
		f.Subtitle = &s.Subtitle
	}
	if s.Status != nil {
		f.Active = &s.Status.Active
		f.Error = &s.Status.Error
		f.Interrupted = &s.Status.Interrupted
	}
	f.Unread = &s.Unread
	f.UnreadToken = &s.UnreadToken
	// started_at is the runner's SetRunning stamp — the persisted "this session
	// was actually observed running" proxy (legacy store.go:40). It is the
	// only source of this fact; dropping it (as the initial cutover did) leaves
	// started_at null forever, breaking the CLI/UI started_at field and the
	// wait "ever alive" check (wait_pure.go:149).
	if s.StartedAt != "" {
		if t, perr := time.Parse(time.RFC3339, s.StartedAt); perr == nil {
			at := centralstore.UnixMillis(t.UnixMilli())
			f.StartedAt.Set = &at
		}
	}
	if s.ExitCode != nil {
		f.ExitCode.Set = s.ExitCode
	}
	if s.ExitedAt != "" {
		if t, perr := time.Parse(time.RFC3339, s.ExitedAt); perr == nil {
			at := centralstore.UnixMillis(t.UnixMilli())
			f.ExitedAt.Set = &at
		}
	}
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
	LeaseFile         *os.File
}

type runnerLaunchResult struct {
	Endpoint  string
	PID       int
	Wait      <-chan error
	Terminate func(context.Context) error
}

type productionRunnerSpawner struct {
	GmuxBin        string
	ResolveDir     func(centralstore.Session) (string, error)
	ResolveCommand func(centralstore.Session) []string
	Launch         func(context.Context, runnerLaunchRequest) (runnerLaunchResult, error)
	ReadyTimeout   time.Duration
	// prepareBeforeRemove is a test-only schedule seam for replacement races.
	prepareBeforeRemove func(string)
	mu                  sync.Mutex
	launched            map[string]runnerLaunchResult
}

func (s *productionRunnerSpawner) Spawn(ctx context.Context, row centralstore.Session) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	legacy := compatSession{ID: string(row.ID), Adapter: row.Adapter, ConversationRef: row.ConversationRef, Cwd: row.CWD, WorkspaceRoot: row.WorkspaceRoot, Remotes: row.Remotes, Command: append([]string(nil), row.Command...)}
	var cwd string
	var err error
	if s.ResolveDir != nil {
		cwd, err = s.ResolveDir(row)
	} else {
		return "", fmt.Errorf("runner spawn: directory resolver unavailable")
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
		legacy.Command = discovery.ResolveResumeCommandFor(legacy.Adapter, legacy.ConversationRef)
	}
	if len(legacy.Command) == 0 {
		return "", fmt.Errorf("runner spawn: session %s is not resumable", row.ID)
	}
	endpoint := filepath.Join(paths.SessionSocketDir(), legacy.ID+".sock")
	// A retained session may predate the socket lease protocol and therefore
	// have a refused socket but no lock-file history. BindSocket deliberately
	// will not remove that shape on its own, because an ordinary runner cannot
	// know whether an unleased owner is starting concurrently. Resume has the
	// stronger identity contract and prepares the exact canonical endpoint:
	// pin, probe, and conditionally remove only the refused inode, then leave
	// lease history for the child. A live occupant aborts before any process (or
	// session-scoped state) is created.
	prepared, err := prepareResumeSocket(endpoint, s.prepareBeforeRemove)
	if err != nil {
		return "", fmt.Errorf("runner spawn: prepare socket: %w", err)
	}
	launch := s.Launch
	if launch == nil {
		launch = launchRunnerProcess
	}
	leaseFile, err := prepared.lease.DuplicateForExec()
	if err != nil {
		prepared.rollback()
		return "", fmt.Errorf("runner spawn: transfer socket lease: %w", err)
	}
	result, err := launch(ctx, runnerLaunchRequest{GmuxBin: s.GmuxBin, Command: legacy.Command, CWD: cwd, ResumeID: legacy.ID, InitialCols: value16(row.TerminalCols), Rows: value16(row.TerminalRows), Endpoint: endpoint, LeaseFile: leaseFile})
	_ = leaseFile.Close()
	if err != nil {
		prepared.rollback()
		return "", err
	}
	// cmd.Start is the ownership-transfer point. Keep the exact sidecar lease
	// held until then: failed launch can remove the file it created without a
	// pathname reacquisition race, while a successful child cannot acquire and
	// bind until the parent publishes the prepared history here.
	prepared.transfer()
	if result.Endpoint == "" {
		result.Endpoint = endpoint
	}
	if result.Terminate == nil {
		return "", fmt.Errorf("runner spawn: launch result has no termination handle")
	}
	// A real process launch returns a Wait channel. Do not hand the endpoint to
	// registration until that process has actually bound its socket; cmd.Start
	// only confirms exec and otherwise races the immediate /events request.
	if result.Wait != nil {
		readyTimeout := s.ReadyTimeout
		if readyTimeout <= 0 {
			readyTimeout = 5 * time.Second
		}
		if err := waitRunnerSocket(ctx, result.Endpoint, result.Wait, readyTimeout); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			cleanupErr := result.Terminate(cleanupCtx)
			cancel()
			if cleanupErr != nil {
				return "", errors.Join(err, fmt.Errorf("runner spawn cleanup: %w", cleanupErr))
			}
			return "", err
		}
	}
	s.mu.Lock()
	if s.launched == nil {
		s.launched = make(map[string]runnerLaunchResult)
	}
	s.launched[result.Endpoint] = result
	s.mu.Unlock()
	return result.Endpoint, nil
}

type preparedResumeSocket struct {
	lease   *socklease.Lease
	created bool
}

func (p *preparedResumeSocket) transfer() {
	_ = p.lease.ReleaseForTransfer()
	p.lease = nil
}

func (p *preparedResumeSocket) rollback() {
	if p.created {
		_ = p.lease.Release()
	} else {
		_ = p.lease.ReleaseKeepingLockFile()
	}
	p.lease = nil
}

func prepareResumeSocket(endpoint string, beforeRemove func(string)) (*preparedResumeSocket, error) {
	if err := socklease.RequireOwnedDir(filepath.Dir(endpoint)); err != nil {
		return nil, err
	}
	lease, err := socklease.Acquire(endpoint)
	if err != nil {
		return nil, err
	}
	prepared := &preparedResumeSocket{lease: lease, created: lease.CreatedLockFile()}
	fail := func(err error, discardHistory bool) (*preparedResumeSocket, error) {
		if prepared.created || discardHistory {
			_ = lease.Release()
		} else {
			_ = lease.ReleaseKeepingLockFile()
		}
		return nil, err
	}

	if _, err := os.Lstat(endpoint); errors.Is(err, os.ErrNotExist) {
		return prepared, nil
	} else if err != nil {
		return fail(err, false)
	}
	pin, err := socklease.PinSocket(endpoint)
	if err != nil {
		return fail(err, false)
	}
	defer pin.Close()
	if err := socklease.ProbeRefusedPinned(pin, 200*time.Millisecond); err != nil {
		// A successful connection is an unleased live owner, so existing lease
		// history describes an older generation and must not survive either.
		return fail(err, errors.Is(err, socklease.ErrSocketLive))
	}
	if beforeRemove != nil {
		beforeRemove(endpoint)
	}
	if err := lease.RemoveSocket(pin); err != nil {
		return fail(err, false)
	}
	return prepared, nil
}

func waitRunnerSocket(ctx context.Context, endpoint string, exited <-chan error, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		conn, err := net.DialTimeout("unix", endpoint, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case waitErr, ok := <-exited:
			if !ok || waitErr == nil {
				return fmt.Errorf("runner spawn: process exited before socket became ready")
			}
			return fmt.Errorf("runner spawn: process exited before socket became ready: %w", waitErr)
		case <-ticker.C:
		case <-deadline.C:
			return fmt.Errorf("runner spawn: socket %s not ready after %s: %w", endpoint, timeout, err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
	if req.LeaseFile != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, req.LeaseFile)
		cmd.Env = append(cmd.Env, "_GMXINTERNAL_SOCKET_LEASE_FD=3")
	}
	// Stdio is wired explicitly, and to nothing.
	//
	// A runner outlives the daemon that spawned it, so a runner holding the
	// daemon's log descriptor would keep writing to whatever inode it names --
	// after one rotation the archive, after the next an unlinked file. exec
	// already gives a nil field /dev/null, and the log descriptors are marked
	// close-on-exec, but a runner is exactly the long-lived child where the
	// invariant matters, so say it here rather than leave it to two defaults.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		if info, statErr := os.Stat(req.CWD); req.CWD != "" && (statErr != nil || !info.IsDir()) {
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
