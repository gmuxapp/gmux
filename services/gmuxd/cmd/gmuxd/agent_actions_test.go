package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionmeta"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// ── test scaffolding ────────────────────────────────────────────────────────
//
// The deps struct is the I/O boundary, so every schedule below is driven
// explicitly: outcomes are fed on a channel the test owns, and timers are
// handed out to the test instead of firing on the wall clock. Nothing here
// sleeps waiting for a race to happen.
//
// The harness models the REAL subscribe ordering, which is what makes the
// "subscribe before deliver" property testable rather than assumed:
//
//   - publish() routes an outcome to the seed while no subscription exists and
//     to the event channel afterwards. Pre-subscribe publications collapse
//     into the seed's single latest row per session, exactly as production's
//     row-version watermark suppresses events already reflected in the seed.
//     A schedule whose edges are published before the subscription is
//     therefore invisible as edges — which is the whole hazard.
//   - sendPrompt refuses to deliver when no subscription exists. Moving the
//     subscribe call after delivery fails loudly instead of quietly losing
//     edges.
//
// Generations are modelled too: the harness's live runtime carries
// harnessGeneration, and status outcomes are stamped with it unless a test
// deliberately uses another.

type fakeTimer struct {
	d  time.Duration
	ch chan time.Time
}

func (t *fakeTimer) fire() { t.ch <- time.Now() }

type promptCall struct{ endpoint, incarnation, prompt, delivery, require string }

type agentHarness struct {
	store    *centralstore.Store
	outcomes chan sessioncoord.Outcome
	timers   chan *fakeTimer
	prompts  chan promptCall
	cancels  chan string
	resumes  chan centralstore.SessionID

	resumeErr  error
	promptErr  error
	cancelErr  error
	guardError func(ctx context.Context, row centralstore.Session) (int, string, string)
	subErr     error
	subs       atomic.Int64
	released   atomic.Int64
	// onPrompt runs inside the sendPrompt call, which is where a genuinely
	// concurrent turn (one that starts and ends before the runner answers)
	// has to be published from.
	onPrompt func()
	// blockPrompt/blockCancel make the runner call wait for its context,
	// modelling a runner wedged on a PTY write.
	blockPrompt bool
	blockCancel bool
	// frame stands in for the turn frame gmuxd retains for this session's live
	// generation — the adapter's own assertion about its turns. nil is the
	// ordinary case for a session whose adapter asserts nothing (a shell, a
	// hook-driven agent, a version-skewed runner): every close is then served
	// result-free.
	frame *sessioncoord.TurnFrame
	// frameReads counts retained-frame lookups, so a test can prove a
	// non-completed close never even asks for a result.
	frameReads atomic.Int64

	mu         sync.Mutex
	subscribed bool
	// owners maps an endpoint pathname onto the incarnation that owns it right
	// now. An endpoint with no entry is unowned, i.e. the fake transport
	// accepts whatever the handler pinned; a test that models a replacement
	// taking over a pathname sets one, and a semantic call naming anybody else
	// is then refused with zero bytes delivered, exactly as the runner does.
	owners map[string]string
	// refused counts semantic calls the fake transport turned away for naming
	// the wrong incarnation, so a test can assert a non-delivery positively
	// rather than by the absence of a prompt.
	refused int
	seed    map[centralstore.SessionID]sessioncoord.Outcome
	liveFn  func(centralstore.SessionID) (sessioncoord.Runtime, bool)
}

// setLive/currentLive guard the registry stand-in: both the test goroutine and
// the handler goroutine change it (a resume installs a new generation), so the
// swap is synchronized rather than relying on channel ordering.
func (h *agentHarness) setLive(fn func(centralstore.SessionID) (sessioncoord.Runtime, bool)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.liveFn = fn
}

func (h *agentHarness) currentLive(id centralstore.SessionID) (sessioncoord.Runtime, bool) {
	h.mu.Lock()
	fn := h.liveFn
	h.mu.Unlock()
	return fn(id)
}

// harnessGeneration is the registry generation of the harness's live runtime,
// and harnessIncarnation the identity of the runner process behind it.
const (
	harnessGeneration  = uint64(7)
	harnessIncarnation = "inc-A"
)

func newAgentHarness(t *testing.T, rows ...centralstore.NewSession) *agentHarness {
	t.Helper()
	st, err := centralstore.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, row := range rows {
		if row.CreatedAt == 0 {
			row.CreatedAt = centralstore.UnixMillis(1)
		}
		if _, _, err := st.InsertSession(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	h := &agentHarness{
		store:    st,
		outcomes: make(chan sessioncoord.Outcome, 16),
		timers:   make(chan *fakeTimer, 8),
		prompts:  make(chan promptCall, 8),
		cancels:  make(chan string, 8),
		resumes:  make(chan centralstore.SessionID, 8),
		seed:     map[centralstore.SessionID]sessioncoord.Outcome{},
		owners:   map[string]string{},
	}
	h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) {
		return sessioncoord.Runtime{SessionID: "s", Endpoint: "/tmp/s.sock", Generation: harnessGeneration, Incarnation: harnessIncarnation}, true
	})
	// Seed the subscription from the inserted rows, the way production's
	// atomic seed does: liveness comes from the registry (here: exited ⇒ dead)
	// and the row carries the generation-scoped status facts.
	for _, row := range rows {
		sess, ok, err := st.Session(context.Background(), row.ID)
		if err != nil || !ok {
			t.Fatalf("seed read %s: %v", row.ID, err)
		}
		alive := row.ExitedAt == nil
		gen := uint64(0)
		if alive {
			gen = harnessGeneration
		}
		copyRow := sess
		h.seed[row.ID] = sessioncoord.Outcome{Type: sessioncoord.OutcomeUpserted, ID: row.ID, Session: &copyRow, Alive: alive, Generation: gen}
	}
	return h
}

// setOwner installs the incarnation that owns an endpoint pathname from now on.
// Called from a seam that runs between the handler's final runtime read and the
// runner call, it models a replacement runner binding the same pathname.
func (h *agentHarness) setOwner(endpoint, incarnation string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.owners[endpoint] = incarnation
}

// checkOwner is the fake transport's stand-in for the runner's mandatory
// incarnation check: an action addressed to a process that no longer owns the
// pathname is refused without writing a byte.
func (h *agentHarness) checkOwner(endpoint, incarnation string) error {
	h.mu.Lock()
	owner, known := h.owners[endpoint]
	if incarnation == "" || (known && owner != incarnation) {
		h.refused++
		h.mu.Unlock()
		if incarnation == "" {
			return errors.New("harness: semantic action carried no expected incarnation")
		}
		return fmt.Errorf("%w: %s is owned by %s", discovery.ErrRunnerIncarnationMismatch, endpoint, owner)
	}
	h.mu.Unlock()
	return nil
}

// refusals reports how many semantic calls were turned away for naming the
// wrong runner.
func (h *agentHarness) refusals() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.refused
}

// publish delivers one outcome the way the coordinator would: into the seed
// while nobody is subscribed, onto the event stream afterwards.
// publish delivers one outcome, stamped with the frame retained at THIS moment —
// which is what the coordinator does at apply time. Stamping here (rather than
// letting the handler read the retained frame whenever it gets around to it) is
// what makes a schedule deterministic: a test that installs a close frame before
// the handler has processed the open edge would otherwise race itself.
//
// An outcome that already carries a frame keeps it: that is how a test drives the
// case where the retained frame has moved on to a newer turn.
func (h *agentHarness) publish(o sessioncoord.Outcome) {
	h.mu.Lock()
	if o.Frame == nil {
		o.Frame = h.frame
	}
	h.mu.Unlock()
	h.mu.Lock()
	if !h.subscribed {
		if o.Type == sessioncoord.OutcomeRemoved {
			delete(h.seed, o.ID)
		} else {
			h.seed[o.ID] = o
		}
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	h.outcomes <- o
}

// setFrame installs the retained turn frame the handler will read, the way a
// runner's turn events would have. openTurn/closeTurn are the two shapes every
// test needs.
func (h *agentHarness) setFrame(f *sessioncoord.TurnFrame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.frame = f
}

// openTurn installs a frame describing turn seq as running.
func (h *agentHarness) openTurn(seq uint64, trigger string) {
	h.setFrame(&sessioncoord.TurnFrame{Seq: seq, Current: &sessioncoord.TurnCurrent{TurnSeq: seq, Exchanges: []sessioncoord.TurnExchange{{Ordinal: 1, User: trigger}}}})
}

// closeTurn installs a frame describing turn seq as settled with this result.
func (h *agentHarness) closeTurn(seq uint64, outcome, output string) {
	h.setFrame(&sessioncoord.TurnFrame{Seq: seq + 100, Last: &sessioncoord.TurnClose{
		TurnSeq: seq, Outcome: outcome, Output: output,
	}})
}

// mergedClose is the settled frame of a turn that took THIS harness's injection
// and then closed: the shape a steer or a merged follow-up must see before it may
// claim the close as its own result.
func mergedClose(seq uint64, outcome, output string) *sessioncoord.TurnFrame {
	return &sessioncoord.TurnFrame{Seq: seq + 100, Last: &sessioncoord.TurnClose{
		TurnSeq: seq, Outcome: outcome, Output: output,
	}}
}

// seedActive overrides this session's seeded status without touching the store,
// which is how a test expresses "the seed disagrees with the store read".
func (h *agentHarness) seedStatus(id string, o sessioncoord.Outcome) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seed[centralstore.SessionID(id)] = o
}

func (h *agentHarness) deps() agentDeps {
	return agentDeps{
		store: h.store,
		subscribe: func(ctx context.Context) ([]sessioncoord.Outcome, <-chan sessioncoord.Outcome, func(), error) {
			h.subs.Add(1)
			if h.subErr != nil {
				return nil, nil, nil, h.subErr
			}
			h.mu.Lock()
			h.subscribed = true
			seed := make([]sessioncoord.Outcome, 0, len(h.seed))
			for _, o := range h.seed {
				seed = append(seed, o)
			}
			h.mu.Unlock()
			return seed, h.outcomes, func() { h.released.Add(1) }, nil
		},
		live: h.currentLive,
		resume: func(_ context.Context, id centralstore.SessionID) (sessioncoord.Runtime, error) {
			h.resumes <- id
			if h.resumeErr != nil {
				return sessioncoord.Runtime{}, h.resumeErr
			}
			// A real Resume converges through Register, which installs the
			// replacement generation in the registry: the revalidation at the
			// delivery boundary must find it.
			rt := sessioncoord.Runtime{SessionID: id, Endpoint: "/tmp/resumed.sock", Generation: harnessGeneration + 1, Incarnation: "inc-resumed"}
			h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) { return rt, true })
			return rt, nil
		},
		sendPrompt: func(ctx context.Context, endpoint, incarnation, prompt, delivery, require string) error {
			if err := h.checkOwner(endpoint, incarnation); err != nil {
				return err
			}
			h.mu.Lock()
			subscribed := h.subscribed
			h.mu.Unlock()
			if !subscribed {
				// Delivering before subscribing loses every edge that lands in
				// the window; it is a bug, not a slower path.
				return errors.New("harness: prompt delivered before any subscription existed")
			}
			h.prompts <- promptCall{endpoint, incarnation, prompt, delivery, require}
			if h.onPrompt != nil {
				h.onPrompt()
			}
			if h.blockPrompt {
				<-ctx.Done()
				return ctx.Err()
			}
			return h.promptErr
		},
		sendCancel: func(ctx context.Context, endpoint, incarnation string) error {
			if err := h.checkOwner(endpoint, incarnation); err != nil {
				return err
			}
			h.cancels <- endpoint
			if h.blockCancel {
				<-ctx.Done()
				return ctx.Err()
			}
			return h.cancelErr
		},
		after: func(d time.Duration) <-chan time.Time {
			tm := &fakeTimer{d: d, ch: make(chan time.Time, 1)}
			h.timers <- tm
			return tm.ch
		},
		frame: func(id centralstore.SessionID) *sessioncoord.TurnFrame {
			h.frameReads.Add(1)
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.frame
		},
		resumeGuardError: func(ctx context.Context, row centralstore.Session) (int, string, string) {
			if h.guardError != nil {
				return h.guardError(ctx, row)
			}
			return 0, "", ""
		},
	}
}

func (h *agentHarness) nextTimer(t *testing.T) *fakeTimer {
	t.Helper()
	select {
	case tm := <-h.timers:
		return tm
	case <-time.After(2 * time.Second):
		t.Fatal("no timer was armed")
		return nil
	}
}

func liveRow(id string, active bool) centralstore.NewSession {
	started := centralstore.UnixMillis(1)
	return centralstore.NewSession{
		ID: centralstore.SessionID(id), Adapter: "pi", Command: []string{"pi"},
		CreatedAt: 1, StartedAt: &started, Active: active, StatusReported: true,
	}
}

func deadRow(id string, activeAtDeath bool) centralstore.NewSession {
	started, exited := centralstore.UnixMillis(1), centralstore.UnixMillis(2)
	code := 0
	return centralstore.NewSession{
		ID: centralstore.SessionID(id), Adapter: "pi", Command: []string{"pi"},
		CreatedAt: 1, StartedAt: &started, ExitedAt: &exited, ExitCode: &code,
		Active: activeAtDeath, StatusReported: activeAtDeath,
	}
}

func statusOutcome(id string, active, errored, interrupted bool) sessioncoord.Outcome {
	return genStatusOutcome(id, harnessGeneration, active, errored, interrupted)
}

func genStatusOutcome(id string, gen uint64, active, errored, interrupted bool) sessioncoord.Outcome {
	row := &centralstore.Session{
		ID: centralstore.SessionID(id), StatusReported: true,
		Active: active, Error: errored, Interrupted: interrupted,
	}
	return sessioncoord.Outcome{Type: sessioncoord.OutcomeUpserted, ID: row.ID, Session: row, Alive: true, Generation: gen}
}

// resumedStatusOutcome is a status report from the generation a transparent
// resume installed: the wait pins the generation it delivered into, so a resumed
// prompt's turn must be reported for the NEW generation.
func resumedStatusOutcome(id string, active, errored, interrupted bool) sessioncoord.Outcome {
	return genStatusOutcome(id, harnessGeneration+1, active, errored, interrupted)
}

func deathOutcome(id string) sessioncoord.Outcome {
	row := &centralstore.Session{ID: centralstore.SessionID(id), StatusReported: true, Active: true}
	return sessioncoord.Outcome{Type: sessioncoord.OutcomeUpserted, ID: row.ID, Session: row, Alive: false}
}

func promptBodyJSON(t *testing.T, body map[string]any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/s/prompt", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

type recorded struct {
	code int
	body map[string]any
}

func (r recorded) data() map[string]any {
	d, _ := r.body["data"].(map[string]any)
	return d
}
func (r recorded) errCode() string {
	e, _ := r.body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}
func (r recorded) errMessage() string {
	e, _ := r.body["error"].(map[string]any)
	s, _ := e["message"].(string)
	return s
}

func runPrompt(t *testing.T, h *agentHarness, req *http.Request) recorded {
	t.Helper()
	rec := httptest.NewRecorder()
	handleAgentPromptCentral(rec, req, h.deps(), "s")
	return parseRecorded(t, rec)
}

func parseRecorded(t *testing.T, rec *httptest.ResponseRecorder) recorded {
	t.Helper()
	out := recorded{code: rec.Code}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out.body); err != nil {
			t.Fatalf("response body %q: %v", rec.Body.String(), err)
		}
	}
	return out
}

// runPromptAsync runs the handler on its own goroutine so the test can drive
// its schedule while it is blocked in the fused wait.
func runPromptAsync(t *testing.T, h *agentHarness, req *http.Request) func() recorded {
	t.Helper()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleAgentPromptCentral(rec, req, h.deps(), "s")
	}()
	return func() recorded {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("handler did not return")
		}
		return parseRecorded(t, rec)
	}
}

// ── intent → mechanism ──────────────────────────────────────────────────────

func TestPromptModeMapsToRunnerMechanism(t *testing.T) {
	for _, tc := range []struct {
		mode, delivery, require string
		active                  bool
	}{
		// Each mode is exercised against the activity it is defined for:
		// prompt against an idle agent, steer and follow-up against a
		// running turn.
		{modePrompt, runnerDeliveryNow, runnerRequireInactive, false},
		{modeFollowUp, runnerDeliveryAfterTurn, runnerRequireAny, true},
		{modeSteer, runnerDeliveryNow, runnerRequireActive, true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			h := newAgentHarness(t, liveRow("s", tc.active))
			get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": tc.mode, "wait": false}))
			call := <-h.prompts
			if !tc.active {
				h.nextTimer(t)
				h.publish(statusOutcome("s", true, false, false))
			}
			got := get()
			if got.code != http.StatusAccepted {
				t.Fatalf("code=%d body=%v", got.code, got.body)
			}
			if call.delivery != tc.delivery || call.require != tc.require || call.prompt != "hi" {
				t.Fatalf("got %+v", call)
			}
			if call.endpoint != "/tmp/s.sock" {
				t.Fatalf("endpoint %q", call.endpoint)
			}
		})
	}
}

// A steer or follow-up into a running turn knows only that bytes were
// delivered; ADR 0027 forbids calling that acceptance.
func TestActiveActionsReportDeliveredNotAccepted(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", true))
	for _, mode := range []string{modeSteer, modeFollowUp} {
		got := runPrompt(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": mode, "wait": false}))
		if got.data()["admission"] != admissionDelivered {
			t.Fatalf("%s: %v", mode, got.body)
		}
		<-h.prompts
	}
}

// ── admission ──────────────────────────────────────────────────────────────

// The whole point of subscribing before delivery: a turn that starts AND ends
// while the runner call is still in flight must still be seen.
//
// The schedule is published from INSIDE sendPrompt, so it is a real causal
// race, and it is also the mutation test: moving the subscribe call after
// delivery makes the harness refuse the delivery outright, and even if it did
// not, both edges would collapse into the seed as a single inactive row and the
// wait would end in admission_timeout.
func TestSubscribeBeforeDeliverCatchesAnInstantTurn(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	h.onPrompt = func() {
		h.publish(statusOutcome("s", true, false, false))
		h.publish(statusOutcome("s", false, false, false))
	}
	got := runPrompt(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	if got.code != http.StatusOK {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
	if got.data()["admission"] != admissionAccepted || got.data()["outcome"] != outcomeCompleted {
		t.Fatalf("body=%v", got.body)
	}
	if _, has := got.data()["output"]; has {
		t.Fatal("this slice must not claim to carry conversation output")
	}
}

// A status write that lands between an earlier store read and the subscription
// is invisible in both places (the read is too early; the event is suppressed
// by the seed's row-version watermark). Baseline therefore comes from the seed.
//
// Stale-inactive: the store row still says active, the seed says inactive. A
// prompt must be admitted on the next real edge rather than treating the stale
// active as "already running".
func TestBaselineComesFromTheSeedNotAnEarlierStoreRead(t *testing.T) {
	t.Run("seed inactive beats an active store row", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", true))
		h.seedStatus("s", statusOutcome("s", false, false, false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt, "wait": false}))
		<-h.prompts
		h.nextTimer(t) // an admission window at all proves the seed won
		h.publish(statusOutcome("s", true, false, false))
		if got := get(); got.data()["admission"] != admissionAccepted {
			t.Fatalf("body=%v", got.body)
		}
	})
	t.Run("seed active beats an inactive store row", func(t *testing.T) {
		// Store says idle, seed says a turn is running: a steer's wait must
		// resolve on that turn's closure, not wait for a fresh edge.
		h := newAgentHarness(t, liveRow("s", false))
		h.seedStatus("s", statusOutcome("s", true, false, false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "stop", "mode": modeSteer}))
		<-h.prompts
		h.publish(statusOutcome("s", false, false, false))
		if got := get(); got.data()["outcome"] != outcomeCompleted {
			t.Fatalf("body=%v", got.body)
		}
	})
	t.Run("acceptance-required completion is gated on admission", func(t *testing.T) {
		// The nastiest combination: a plain prompt whose seed baseline is
		// ACTIVE (the runner's precondition was evaluated against state this
		// process has not caught up with). The closing edge of that turn is
		// not this prompt's completion — nothing of ours has been admitted —
		// so the wait must ignore it and resolve only on a real
		// admission+closure pair.
		h := newAgentHarness(t, liveRow("s", false))
		h.seedStatus("s", statusOutcome("s", true, false, false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
		<-h.prompts
		h.nextTimer(t) // admission window
		h.publish(statusOutcome("s", false, true, false))
		select {
		case <-h.timers:
			t.Fatal("unexpected timer: the prior turn's closure resolved the wait")
		case <-time.After(20 * time.Millisecond):
		}
		h.publish(statusOutcome("s", true, false, false))
		h.publish(statusOutcome("s", false, false, false))
		got := get()
		if got.code != http.StatusOK || got.data()["admission"] != admissionAccepted || got.data()["outcome"] != outcomeCompleted {
			t.Fatalf("code=%d body=%v", got.code, got.body)
		}
	})
	t.Run("seed active for another generation is not our baseline", func(t *testing.T) {
		// The seed's active row belongs to a generation that is not the one
		// receiving these bytes (the classic case: the dead predecessor of a
		// session this request resumed). It must not be read as current.
		h := newAgentHarness(t, liveRow("s", true))
		h.seedStatus("s", genStatusOutcome("s", harnessGeneration-1, true, false, false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modeFollowUp, "wait": false}))
		<-h.prompts
		// A follow-up with an inactive baseline is acceptance-bearing, so an
		// admission window proves the stale active was discarded.
		h.nextTimer(t)
		h.publish(statusOutcome("s", true, false, false))
		if got := get(); got.data()["admission"] != admissionAccepted {
			t.Fatalf("body=%v", got.body)
		}
	})
}

func TestDetachedInactivePromptWaitsForAFreshTurn(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt, "wait": false}))
	<-h.prompts
	tm := h.nextTimer(t)
	if tm.d != defaultAdmissionWindow {
		t.Fatalf("admission window %v", tm.d)
	}
	// A repeated inactive report is not acceptance.
	h.publish(statusOutcome("s", false, false, false))
	h.publish(statusOutcome("s", true, false, false))
	got := get()
	if got.code != http.StatusAccepted || got.data()["admission"] != admissionAccepted {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
}

// An admission timeout is indeterminate: the bytes went in. It must not read
// as retryable, and the daemon must not resend anything.
func TestAdmissionTimeoutIsIndeterminateAndDoesNotRedeliver(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	<-h.prompts
	h.nextTimer(t).fire()
	got := get()
	if got.code != http.StatusGatewayTimeout || got.errCode() != codeAdmissionTimeout {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
	msg := got.errMessage()
	if !strings.Contains(msg, "indeterminate") || !strings.Contains(msg, "duplicate") {
		t.Fatalf("message must warn about duplication: %q", msg)
	}
	if strings.Contains(msg, "retry the prompt") || strings.Contains(msg, "safe to retry") {
		t.Fatalf("message must not invite a blind retry: %q", msg)
	}
	select {
	case extra := <-h.prompts:
		t.Fatalf("prompt was delivered twice: %+v", extra)
	default:
	}
}

// ── fused completion ───────────────────────────────────────────────────────

func TestOneSubscriptionSpansAdmissionThroughCompletion(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		errored, interrupted bool
		wantOutcome          string
	}{
		{"completed", false, false, outcomeCompleted},
		{"terminal error", true, false, outcomeError},
		{"interrupted", false, true, outcomeInterrupted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgentHarness(t, liveRow("s", false))
			get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
			<-h.prompts
			h.nextTimer(t) // admission window, never fired
			h.publish(statusOutcome("s", true, false, false))
			// Active+error is still active: an adapter reporting a
			// rate-limit/retry condition has not finished the turn.
			h.publish(statusOutcome("s", true, true, false))
			h.publish(statusOutcome("s", false, tc.errored, tc.interrupted))
			got := get()
			if got.code != http.StatusOK || got.data()["outcome"] != tc.wantOutcome {
				t.Fatalf("code=%d body=%v", got.code, got.body)
			}
			if got.data()["admission"] != admissionAccepted {
				t.Fatalf("body=%v", got.body)
			}
			// ONE subscription spans pre-delivery, admission and completion.
			// Resubscribing between phases is what would let a fast turn slip
			// through the gap.
			if n := h.subs.Load(); n != 1 {
				t.Fatalf("subscribed %d times", n)
			}
		})
	}
}

func TestActiveErrorDoesNotResolveTheWait(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt, "timeout_seconds": 30}))
	<-h.prompts
	h.nextTimer(t) // admission
	h.publish(statusOutcome("s", true, false, false))
	exec := h.nextTimer(t)
	if exec.d != 30*time.Second {
		t.Fatalf("execution timeout %v", exec.d)
	}
	h.publish(statusOutcome("s", true, true, false))
	h.publish(statusOutcome("s", true, true, false))
	select {
	case <-h.prompts:
		t.Fatal("unexpected second delivery")
	case <-time.After(20 * time.Millisecond):
	}
	// Only the execution deadline can end this wait; nothing above did.
	exec.fire()
	got := get()
	// 504, not 408: 408 invites a replay, and replaying a prompt duplicates it.
	if got.code != http.StatusGatewayTimeout || got.data()["outcome"] != "timeout" || got.data()["cause"] != codeExecutionTimeout {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
}

// The execution deadline starts at the execution boundary (acceptance), not at
// request arrival, and zero means indefinite.
func TestExecutionTimeoutIsSeparateFromAdmissionAndOptional(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt, "timeout_seconds": 0}))
	<-h.prompts
	adm := h.nextTimer(t)
	if adm.d != defaultAdmissionWindow {
		t.Fatalf("admission %v", adm.d)
	}
	h.publish(statusOutcome("s", true, false, false))
	select {
	case tm := <-h.timers:
		t.Fatalf("timeout_seconds=0 must arm no execution deadline, got %v", tm.d)
	case <-time.After(20 * time.Millisecond):
	}
	h.publish(statusOutcome("s", false, false, false))
	if got := get(); got.data()["outcome"] != outcomeCompleted {
		t.Fatalf("body=%v", got.body)
	}
}

func TestRunnerDeathIsAnErrorOutcomeWithACause(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	<-h.prompts
	h.nextTimer(t)
	h.publish(statusOutcome("s", true, false, false))
	// The registry is the arbiter of death: the generation that received the
	// bytes is gone.
	h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) { return sessioncoord.Runtime{}, false })
	h.publish(deathOutcome("s"))
	got := get()
	if got.code != http.StatusOK || got.data()["outcome"] != outcomeError || got.data()["cause"] != causeRunnerDied {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
}

// A removal can race a replacement registration: the outcome says the row went
// away, but by the time it is delivered a new generation is installed. The
// registry is the arbiter, not the outcome's own liveness stamp.
func TestStaleRemovalWhileTheGenerationLivesIsIgnored(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	<-h.prompts
	h.nextTimer(t)
	h.publish(statusOutcome("s", true, false, false))
	// remove → re-register: the removal is published late, while OUR
	// generation is still the installed live one.
	h.publish(sessioncoord.Outcome{Type: sessioncoord.OutcomeRemoved, ID: "s"})
	h.publish(deathOutcome("s")) // an Alive=false upsert overtaken the same way
	select {
	case <-time.After(20 * time.Millisecond):
	case <-h.timers:
		t.Fatal("unexpected timer")
	}
	h.publish(statusOutcome("s", false, false, false))
	got := get()
	if got.code != http.StatusOK || got.data()["outcome"] != outcomeCompleted {
		t.Fatalf("a stale removal must not become a death: code=%d body=%v", got.code, got.body)
	}
}

// The same signal, but the registry confirms our generation is gone (or was
// replaced): that is a death, and a replacement generation's turns are never
// ours.
func TestGenerationAwareDeathAndForeignGenerations(t *testing.T) {
	t.Run("our generation is gone", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
		<-h.prompts
		h.nextTimer(t)
		h.publish(statusOutcome("s", true, false, false))
		h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) {
			return sessioncoord.Runtime{}, false
		})
		h.publish(sessioncoord.Outcome{Type: sessioncoord.OutcomeRemoved, ID: "s"})
		got := get()
		if got.data()["outcome"] != outcomeError || got.data()["cause"] != causeRunnerDied {
			t.Fatalf("body=%v", got.body)
		}
	})
	// A foreign live row while OUR generation is still installed is an ordering
	// artefact, not evidence: attribution is filtered, the wait continues.
	t.Run("foreign turn while our generation still lives is ignored", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
		<-h.prompts
		h.nextTimer(t)
		h.publish(statusOutcome("s", true, false, false))
		// The foreign generation's turn FAILS. Attributing it would report this
		// prompt as errored; ignoring it is the only correct reading.
		h.publish(genStatusOutcome("s", harnessGeneration+1, true, false, false))
		h.publish(genStatusOutcome("s", harnessGeneration+1, false, true, false))
		select {
		case <-h.timers:
			t.Fatal("unexpected timer")
		case <-time.After(20 * time.Millisecond):
		}
		h.publish(statusOutcome("s", false, false, false))
		if got := get(); got.data()["outcome"] != outcomeCompleted {
			t.Fatalf("another generation's failure was attributed to this prompt: %v", got.body)
		}
	})
}

// TestReplacementRunnerNeverExecutesAPromptPinnedToItsPredecessor closes the
// window the daemon cannot close on its own.
//
// Schedule: the handler re-reads the runtime, pins (endpoint, incarnation), and
// only then calls the runner. Between those two points — driven here from the
// `live` seam itself, which hands back the predecessor and makes the replacement
// the pathname's owner as it returns — the pinned generation exits and a
// replacement binds the SAME pathname. Without an identity on the request the
// replacement would execute the prompt while the wait, still pinned to the
// predecessor, reported runner_died: a real side effect reported as an
// indeterminate failure, which a blind retry then duplicates.
//
// What must hold instead: ZERO bytes reach the replacement, and the caller gets
// a guaranteed-non-delivery code that is safe to retry.
func TestReplacementRunnerNeverExecutesAPromptPinnedToItsPredecessor(t *testing.T) {
	for _, tc := range []struct{ name, action string }{{"prompt", "prompt"}, {"cancel", "cancel"}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgentHarness(t, liveRow("s", true))
			// live is the barrier: the handler re-reads the runtime immediately
			// before delivering, and this hands back the predecessor while
			// making the replacement the pathname's owner from that instant on
			// — the exact window the runner-side check exists to close.
			reads := 0
			stale := sessioncoord.Runtime{SessionID: "s", Endpoint: "/tmp/s.sock",
				Generation: harnessGeneration, Incarnation: harnessIncarnation}
			deps := h.deps()
			deps.live = func(centralstore.SessionID) (sessioncoord.Runtime, bool) {
				reads++
				if reads >= 2 {
					h.setOwner("/tmp/s.sock", "inc-B")
				}
				return stale, true
			}
			rec := httptest.NewRecorder()
			if tc.action == "prompt" {
				handleAgentPromptCentral(rec, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modeSteer}), deps, "s")
			} else {
				// Cancel reads the runtime once, so the takeover is already in
				// place when it delivers.
				h.setOwner("/tmp/s.sock", "inc-B")
				handleAgentCancelCentral(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/s/cancel", nil), deps, "s")
			}
			got := parseRecorded(t, rec)
			if got.code != http.StatusConflict || got.errCode() != codeIncarnationMismatch {
				t.Fatalf("code=%d body=%v, want 409/%s", got.code, got.body, codeIncarnationMismatch)
			}
			if !strings.Contains(got.errMessage(), "safe to retry") {
				t.Fatalf("a guaranteed non-delivery must say so: %q", got.errMessage())
			}
			if h.refusals() != 1 {
				t.Fatalf("refusals=%d, want 1 (the replacement must have been asked and said no)", h.refusals())
			}
			select {
			case p := <-h.prompts:
				t.Fatalf("bytes reached the replacement runner: %+v", p)
			case ep := <-h.cancels:
				t.Fatalf("an interrupt reached the replacement runner at %s", ep)
			default:
			}
		})
	}
}

// An unidentifiable runner cannot be addressed safely at all: there is no
// expectation to send, and sending none would reopen the replacement window.
// Such a runner also predates the semantic routes, so it reports the same
// version fact the 404 path does.
func TestUnidentifiedRunnerIsRefusedBeforeDelivery(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", true))
	h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) {
		return sessioncoord.Runtime{SessionID: "s", Endpoint: "/tmp/s.sock", Generation: harnessGeneration}, true
	})
	got := runPrompt(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modeSteer}))
	if got.errCode() != codeRunnerOutdated {
		t.Fatalf("code=%d body=%v, want %s", got.code, got.body, codeRunnerOutdated)
	}
	rec := httptest.NewRecorder()
	handleAgentCancelCentral(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/s/cancel", nil), h.deps(), "s")
	if cancelGot := parseRecorded(t, rec); cancelGot.errCode() != codeRunnerOutdated {
		t.Fatalf("cancel code=%d body=%v", cancelGot.code, cancelGot.body)
	}
	select {
	case <-h.prompts:
		t.Fatal("a prompt was delivered to a runner nobody could identify")
	case <-h.cancels:
		t.Fatal("a cancel was delivered to a runner nobody could identify")
	default:
	}
}

// A takeover/restart may publish the replacement's rows and NO removal or
// Alive=false outcome for the predecessor at all. The replacement upsert is
// then the only terminal evidence there will ever be, and filtering it as
// foreign attribution left a caller without timeout_seconds waiting forever for
// a turn that could never be reported.
func TestReplacementUpsertIsTerminalEvidenceOnItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		wait bool
	}{{"synchronous", true}, {"detached", false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgentHarness(t, liveRow("s", false))
			body := map[string]any{"prompt": "hi", "mode": modePrompt}
			if !tc.wait {
				body["wait"] = false
			}
			get := runPromptAsync(t, h, promptBodyJSON(t, body))
			<-h.prompts
			h.nextTimer(t)
			// The registry has swapped to the replacement: our generation is
			// gone. No death or removal outcome is ever published.
			h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) {
				return sessioncoord.Runtime{SessionID: "s", Endpoint: "/tmp/new.sock", Generation: harnessGeneration + 1, Incarnation: "inc-new"}, true
			})
			h.publish(genStatusOutcome("s", harnessGeneration+1, true, false, false))
			got := get()
			if tc.wait {
				if got.code != http.StatusOK || got.data()["outcome"] != outcomeError || got.data()["cause"] != causeRunnerDied {
					t.Fatalf("code=%d body=%v", got.code, got.body)
				}
				return
			}
			if got.code != http.StatusBadGateway || got.errCode() != causeRunnerDied {
				t.Fatalf("code=%d body=%v", got.code, got.body)
			}
		})
	}
}

// The runtime is looked up before the resume decision and before the
// subscription, and socket pathnames are reusable: a replacement registering in
// that window would otherwise make the pinned generation name the OLD runner
// while the bytes reach the new one -- surfacing as admission_timeout for a
// prompt that demonstrably ran.
func TestReplacementBetweenLookupAndDeliveryIsRepinned(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	var looks atomic.Int64
	h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) {
		if looks.Add(1) == 1 {
			return sessioncoord.Runtime{SessionID: "s", Endpoint: "/tmp/old.sock", Generation: harnessGeneration, Incarnation: "inc-old"}, true
		}
		return sessioncoord.Runtime{SessionID: "s", Endpoint: "/tmp/new.sock", Generation: harnessGeneration + 1, Incarnation: "inc-new"}, true
	})
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	call := <-h.prompts
	if call.endpoint != "/tmp/new.sock" {
		t.Fatalf("bytes must go to the generation that is live now: %+v", call)
	}
	h.nextTimer(t)
	// The turn of the generation actually delivered to must be attributed to
	// this request, not filtered as foreign.
	h.publish(genStatusOutcome("s", harnessGeneration+1, true, false, false))
	h.publish(genStatusOutcome("s", harnessGeneration+1, false, false, false))
	got := get()
	if got.code != http.StatusOK || got.data()["admission"] != admissionAccepted || got.data()["outcome"] != outcomeCompleted {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
}

// The generation vanishing before a single byte is sent is safe to retry, and
// must not be dressed up as runner_died (which claims bytes are in flight).
func TestRunnerGoneAtTheDeliveryBoundaryDeliversNothing(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	var looks atomic.Int64
	h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) {
		if looks.Add(1) == 1 {
			return sessioncoord.Runtime{SessionID: "s", Endpoint: "/tmp/s.sock", Generation: harnessGeneration, Incarnation: harnessIncarnation}, true
		}
		return sessioncoord.Runtime{}, false
	})
	got := runPrompt(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	if got.code != http.StatusConflict || got.errCode() != "not_running" {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
	if !strings.Contains(got.errMessage(), "nothing was delivered") {
		t.Fatalf("message must state zero delivery: %q", got.errMessage())
	}
	select {
	case call := <-h.prompts:
		t.Fatalf("delivered to a vanished runner: %+v", call)
	default:
	}
	if got.errCode() == causeRunnerDied {
		t.Fatal("zero-byte failure must not read as a death")
	}
}

func TestDeathBeforeAdmissionIsReportedToADetachedCaller(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt, "wait": false}))
	<-h.prompts
	h.nextTimer(t)
	h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) { return sessioncoord.Runtime{}, false })
	h.publish(sessioncoord.Outcome{Type: sessioncoord.OutcomeRemoved, ID: "s"})
	got := get()
	if got.code != http.StatusBadGateway || got.errCode() != causeRunnerDied {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
}

// A steer joins a turn that is already running, so its wait must resolve on
// that turn's closure -- and must not resolve on a stale inactive observation
// that predates the turn the daemon's baseline had not yet seen.
func TestSteerWaitsForTheCurrentTurnClosure(t *testing.T) {
	t.Run("baseline active resolves on the closure", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", true))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "stop that", "mode": modeSteer}))
		<-h.prompts
		h.publish(statusOutcome("s", false, false, true))
		got := get()
		if got.data()["admission"] != admissionDelivered || got.data()["outcome"] != outcomeInterrupted {
			t.Fatalf("body=%v", got.body)
		}
	})
	t.Run("stale inactive does not resolve", func(t *testing.T) {
		// Baseline reads inactive (the runner saw the turn; this row had not
		// caught up). A lone inactive observation must not be mistaken for
		// this turn ending.
		h := newAgentHarness(t, liveRow("s", false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "stop that", "mode": modeSteer}))
		<-h.prompts
		h.publish(statusOutcome("s", false, false, false))
		select {
		case <-h.timers:
			t.Fatal("a steer must not arm an admission window")
		case <-time.After(20 * time.Millisecond):
		}
		h.publish(statusOutcome("s", true, false, false))
		h.publish(statusOutcome("s", false, false, false))
		if got := get(); got.data()["outcome"] != outcomeCompleted {
			t.Fatalf("body=%v", got.body)
		}
	})
}

// A follow-up delivered into a RUNNING turn is merged into that loop by the
// agent (pi's queue): one loop, one turn, one close, and that close's answer is
// the follow-up's. There is no second turn to await — the old two-close model
// (and its queued_turn_unobserved verdict) described a world pi does not
// implement.
func TestFollowUpResolvesOnTheMergedClose(t *testing.T) {
	t.Run("the merged close is this prompt's result", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", true))
		h.openTurn(9, "first ask") // the loop the follow-up will merge into
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "and then", "mode": modeFollowUp}))
		<-h.prompts
		// The source closes the same turn identity this follow-up joined; no
		// instruction event resolves the observer on its own.
		h.setFrame(mergedClose(9, outcomeCompleted, "merged answer"))
		h.publish(statusOutcome("s", false, false, false))
		got := get()
		if got.data()["outcome"] != outcomeCompleted || got.data()["output"] != "merged answer" {
			t.Fatalf("body=%v", got.body)
		}
	})
	t.Run("a failed merged turn carries no answer", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", true))
		h.openTurn(9, "first ask")
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "and then", "mode": modeFollowUp}))
		<-h.prompts
		h.setFrame(mergedClose(9, outcomeError, ""))
		h.publish(statusOutcome("s", false, true, false))
		got := get()
		if got.data()["outcome"] != outcomeError {
			t.Fatalf("body=%v", got.body)
		}
		if _, ok := got.data()["output"]; ok {
			t.Fatalf("failed turn carried a result: %v", got.body)
		}
	})
	t.Run("idle follow-up is admitted like a submit", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "go", "mode": modeFollowUp, "wait": false}))
		<-h.prompts
		h.nextTimer(t)
		h.publish(statusOutcome("s", true, false, false))
		if got := get(); got.data()["admission"] != admissionAccepted {
			t.Fatalf("body=%v", got.body)
		}
	})
}

// ── transparent resume ─────────────────────────────────────────────────────

func TestDeadSessionResumeMatrix(t *testing.T) {
	for _, tc := range []struct {
		mode        string
		wantResume  bool
		wantDeliver bool
		wantCode    int
	}{
		{modePrompt, true, true, http.StatusAccepted},
		{modeFollowUp, true, true, http.StatusAccepted},
		{modeSteer, false, false, http.StatusConflict},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			// Active=true at death: prior-generation evidence, never a
			// precondition on the new generation.
			h := newAgentHarness(t, deadRow("s", true))
			h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) {
				return sessioncoord.Runtime{}, false
			})
			get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": tc.mode, "wait": false}))
			if tc.wantResume {
				if id := <-h.resumes; id != "s" {
					t.Fatalf("resumed %q", id)
				}
			}
			if tc.wantDeliver {
				call := <-h.prompts
				if call.endpoint != "/tmp/resumed.sock" {
					t.Fatalf("prompt must go to the NEW generation: %+v", call)
				}
				// The resumed row has no reported status, so the prompt still
				// waits for a genuine turn -- reported by the generation the
				// resume installed, which is the one the bytes went to.
				h.nextTimer(t)
				h.publish(resumedStatusOutcome("s", true, false, false))
			}
			got := get()
			if got.code != tc.wantCode {
				t.Fatalf("code=%d body=%v", got.code, got.body)
			}
			if !tc.wantResume {
				select {
				case id := <-h.resumes:
					t.Fatalf("%s must never resume (resumed %q)", tc.mode, id)
				default:
				}
				select {
				case call := <-h.prompts:
					t.Fatalf("%s must not deliver to a dead session: %+v", tc.mode, call)
				default:
				}
				if got.errCode() != "not_running" {
					t.Fatalf("code %q", got.errCode())
				}
			} else if got.data()["resumed"] != true {
				t.Fatalf("resume must be reported: %v", got.body)
			}
		})
	}
}

func TestCancelNeverResumesADeadSession(t *testing.T) {
	h := newAgentHarness(t, deadRow("s", true))
	h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) { return sessioncoord.Runtime{}, false })
	rec := httptest.NewRecorder()
	handleAgentCancelCentral(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/s/cancel", nil), h.deps(), "s")
	got := parseRecorded(t, rec)
	if got.code != http.StatusConflict || got.errCode() != "not_running" {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
	select {
	case <-h.resumes:
		t.Fatal("cancel resumed a session")
	case <-h.cancels:
		t.Fatal("cancel was delivered to a dead session")
	default:
	}
}

// Losing the resume race is not an error: the other request installed exactly
// the generation this one wanted, so the prompt proceeds against it -- with no
// second spawn and no second Resume.
func TestConcurrentResumeConvergesOnOneRunner(t *testing.T) {
	h := newAgentHarness(t, deadRow("s", false))
	h.resumeErr = fmt.Errorf("wrapped: %w", sessioncoord.ErrSessionAlive)
	var looks atomic.Int64
	h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) {
		if looks.Add(1) == 1 {
			// Dead on the first look, alive by the time our Resume loses the
			// race to the concurrent one.
			return sessioncoord.Runtime{}, false
		}
		return sessioncoord.Runtime{SessionID: "s", Endpoint: "/tmp/winner.sock", Generation: harnessGeneration + 1, Incarnation: "inc-winner"}, true
	})
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt, "wait": false}))
	<-h.resumes
	call := <-h.prompts
	if call.endpoint != "/tmp/winner.sock" {
		t.Fatalf("prompt must use the winning generation: %+v", call)
	}
	h.nextTimer(t)
	h.publish(resumedStatusOutcome("s", true, false, false))
	if got := get(); got.code != http.StatusAccepted {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
}

func TestConcurrentLifecycleOperationIsBusyNotQueued(t *testing.T) {
	h := newAgentHarness(t, deadRow("s", false))
	h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) { return sessioncoord.Runtime{}, false })
	h.resumeErr = sessioncoord.ErrLifecycleOpInFlight
	got := runPrompt(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	if got.code != http.StatusConflict || got.errCode() != "busy" {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
	select {
	case call := <-h.prompts:
		t.Fatalf("nothing may be delivered without a runner: %+v", call)
	default:
	}
}

// Transparent resume must not be able to succeed where the explicit
// POST /resume would refuse.
func TestResumeGuardBlocksTransparentResume(t *testing.T) {
	h := newAgentHarness(t, deadRow("s", false))
	h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) { return sessioncoord.Runtime{}, false })
	h.guardError = func(context.Context, centralstore.Session) (int, string, string) {
		return http.StatusUnprocessableEntity, "cwd_missing", "gone"
	}
	got := runPrompt(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	if got.code != http.StatusUnprocessableEntity || got.errCode() != "cwd_missing" {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
	select {
	case <-h.resumes:
		t.Fatal("resume attempted despite the guard")
	default:
	}
}

// ── runner error mapping ───────────────────────────────────────────────────

func TestOldRunnerBecomesRunnerOutdated(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	h.promptErr = fmt.Errorf("runner /prompt: %w: answered 404", discovery.ErrRunnerSemanticActionsUnsupported)
	got := runPrompt(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	if got.code != http.StatusBadGateway || got.errCode() != codeRunnerOutdated {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
	if !strings.Contains(got.errMessage(), "restart") {
		t.Fatalf("message must be actionable: %q", got.errMessage())
	}
}

// TestRunnerRefusalCodesArePreserved ensures every stable runner refusal code
// reaches the caller intact. The runner owns whether bytes were delivered;
// re-coding here would either lose that fact or claim something this daemon
// cannot know.
//
// Delivery taxonomy:
//
//	not_running (409)        — zero bytes: child exited before readiness
//	unsupported_adapter (422)— zero bytes: adapter has no semantic actions
//	unsupported_action (422) — zero bytes: adapter cannot express this action
//	not_ready (504)          — zero bytes: readiness deadline expired
//	precondition_failed (409)— zero bytes: activity requirement not met
//	delivery_pending (409)   — zero bytes: prior delivery not yet acknowledged
//	transport_error (500→502)— INDETERMINATE: write failed mid-flight
func TestRunnerRefusalCodesArePreserved(t *testing.T) {
	for _, tc := range []struct {
		status   int
		code     string
		wantHTTP int
	}{
		// Guaranteed non-delivery (safe to retry):
		{http.StatusConflict, "not_running", http.StatusConflict},
		{http.StatusConflict, "precondition_failed", http.StatusConflict},
		{http.StatusConflict, "delivery_pending", http.StatusConflict},
		{http.StatusGatewayTimeout, "not_ready", http.StatusGatewayTimeout},
		{http.StatusUnprocessableEntity, "unsupported_adapter", http.StatusUnprocessableEntity},
		{http.StatusUnprocessableEntity, "unsupported_action", http.StatusUnprocessableEntity},
		// Indeterminate (inspect before retrying):
		{http.StatusInternalServerError, "transport_error", http.StatusBadGateway},
		// A runner rejecting gmuxd's own envelope is a gmux bug, not a
		// caller error.
		{http.StatusBadRequest, "invalid_request", http.StatusInternalServerError},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.code), func(t *testing.T) {
			h := newAgentHarness(t, liveRow("s", true))
			h.promptErr = &discovery.RunnerActionError{Status: tc.status, Code: tc.code, Message: "runner says so"}
			got := runPrompt(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modeSteer}))
			if got.code != tc.wantHTTP || got.errCode() != tc.code {
				t.Fatalf("code=%d body=%v", got.code, got.body)
			}
			if got.errMessage() != "runner says so" {
				t.Fatalf("message %q", got.errMessage())
			}
		})
	}
}

func TestUnreachableRunnerIsNotAnOutdatedRunner(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", true))
	h.promptErr = errors.New("dial unix /tmp/s.sock: no such file")
	got := runPrompt(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modeSteer}))
	if got.code != http.StatusBadGateway || got.errCode() != codeRunnerUnreachable {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
}

// ── request lifetime ───────────────────────────────────────────────────────

// A caller who hangs up before the runner call must cause no delivery at all.
func TestCanceledRequestDeliversNothing(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}).WithContext(ctx)
	rec := httptest.NewRecorder()
	handleAgentPromptCentral(rec, req, h.deps(), "s")
	select {
	case call := <-h.prompts:
		t.Fatalf("delivered after cancellation: %+v", call)
	default:
	}
}

// A caller who hangs up mid-wait gets no response written: the connection is
// gone and the turn keeps running.
func TestCanceledWaitWritesNothingAndLeavesNoSubscription(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	ctx, cancel := context.WithCancel(context.Background())
	req := promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); handleAgentPromptCentral(rec, req, h.deps(), "s") }()
	<-h.prompts
	h.nextTimer(t)
	cancel()
	<-done
	if h.released.Load() != 1 {
		t.Fatalf("subscription released %d times", h.released.Load())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("wrote %q to a hung-up caller", rec.Body.String())
	}
}

// A wedged runner call must not park the request for as long as the client
// tolerates, and its expiry is INDETERMINATE: the runner may have written the
// prompt already, so it can be neither runner_unreachable nor a safe retry.
func TestWedgedRunnerCallHitsTheDeliveryDeadline(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	h.blockPrompt = true
	deps := h.deps()
	deps.deliveryTimeout = 40 * time.Millisecond
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleAgentPromptCentral(rec, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}), deps, "s")
	}()
	<-h.prompts
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a wedged runner call was not bounded")
	}
	got := parseRecorded(t, rec)
	if got.code != http.StatusGatewayTimeout || got.errCode() != codeDeliveryTimeout {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
	msg := got.errMessage()
	if !strings.Contains(msg, "may already have been delivered") || !strings.Contains(msg, "duplicate") {
		t.Fatalf("message must own the indeterminacy: %q", msg)
	}
	if h.released.Load() != 1 {
		t.Fatalf("subscription released %d times", h.released.Load())
	}
	// No admission wait was ever entered, so no timer was armed and nothing is
	// left running.
	select {
	case tm := <-h.timers:
		t.Fatalf("unexpected timer %v", tm.d)
	default:
	}
}

// A cancel that times out must not send the caller hunting for a duplicated
// prompt: nothing about a prompt was in flight.
func TestCancelDeliveryTimeoutTalksAboutTheInterrupt(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", true))
	h.blockCancel = true
	deps := h.deps()
	deps.deliveryTimeout = 40 * time.Millisecond
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleAgentCancelCentral(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/s/cancel", nil), deps, "s")
	}()
	<-h.cancels
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a wedged cancel was not bounded")
	}
	got := parseRecorded(t, rec)
	if got.code != http.StatusGatewayTimeout || got.errCode() != codeDeliveryTimeout {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
	msg := got.errMessage()
	if !strings.Contains(msg, "interrupt") {
		t.Fatalf("message must name the interrupt: %q", msg)
	}
	for _, wrong := range []string{"prompt", "resending", "duplicate"} {
		if strings.Contains(msg, wrong) {
			t.Fatalf("cancel timeout must not talk about %q: %q", wrong, msg)
		}
	}
	// And the prompt wording is still prompt-specific.
	if !strings.Contains(deliveryTimeoutMessage(opPrompt), "prompt") ||
		!strings.Contains(deliveryTimeoutMessage(opPrompt), "duplicate") {
		t.Fatalf("prompt wording regressed: %q", deliveryTimeoutMessage(opPrompt))
	}
}

// A caller who hangs up during the runner call keeps its own semantics: no
// response, and no delivery_timeout claim on their behalf.
func TestCallerCancellationDuringDeliveryWritesNothing(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	h.blockPrompt = true
	ctx, cancel := context.WithCancel(context.Background())
	req := promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); handleAgentPromptCentral(rec, req, h.deps(), "s") }()
	<-h.prompts
	cancel()
	<-done
	if rec.Body.Len() != 0 {
		t.Fatalf("wrote %q to a hung-up caller", rec.Body.String())
	}
	if h.released.Load() != 1 {
		t.Fatalf("subscription released %d times", h.released.Load())
	}
}

// ── validation ─────────────────────────────────────────────────────────────

func TestPromptValidation(t *testing.T) {
	big := strings.Repeat("x", maxInputBytes+1)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"missing prompt", `{"mode":"prompt"}`},
		{"empty prompt", `{"prompt":"","mode":"prompt"}`},
		{"missing mode", `{"prompt":"hi"}`},
		{"unknown mode", `{"prompt":"hi","mode":"steering"}`},
		{"empty mode", `{"prompt":"hi","mode":""}`},
		{"unknown field", `{"prompt":"hi","mode":"prompt","urgent":true}`},
		{"trailing json", `{"prompt":"hi","mode":"prompt"} {"prompt":"again"}`},
		{"prompt not a string", `{"prompt":42,"mode":"prompt"}`},
		{"wait not a bool", `{"prompt":"hi","mode":"prompt","wait":"yes"}`},
		{"negative timeout", `{"prompt":"hi","mode":"prompt","timeout_seconds":-1}`},
		{"absurd timeout", `{"prompt":"hi","mode":"prompt","timeout_seconds":99999999}`},
		{"oversized prompt", `{"prompt":"` + big + `","mode":"prompt"}`},
		{"not json", `hello`},
		{"empty body", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgentHarness(t, liveRow("s", false))
			req := httptest.NewRequest(http.MethodPost, "/v1/sessions/s/prompt", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handleAgentPromptCentral(rec, req, h.deps(), "s")
			got := parseRecorded(t, rec)
			if got.code != http.StatusBadRequest {
				t.Fatalf("code=%d body=%v", got.code, got.body)
			}
			select {
			case call := <-h.prompts:
				t.Fatalf("invalid request reached the runner: %+v", call)
			default:
			}
		})
	}
}

func TestPromptRejectsInvalidUTF8AndWrongMediaType(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/s/prompt", strings.NewReader("{\"prompt\":\"\xff\xfe\",\"mode\":\"prompt\"}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleAgentPromptCentral(rec, req, h.deps(), "s")
	if got := parseRecorded(t, rec); got.code != http.StatusBadRequest || !strings.Contains(got.errMessage(), "UTF-8") {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/s/prompt", strings.NewReader(`{"prompt":"hi","mode":"prompt"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec = httptest.NewRecorder()
	handleAgentPromptCentral(rec, req, h.deps(), "s")
	if got := parseRecorded(t, rec); got.code != http.StatusBadRequest {
		t.Fatalf("media type accepted: %v", got.body)
	}
}

func TestWaitDefaultsToTrueWhenOmitted(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", true))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modeSteer}))
	<-h.prompts
	h.publish(statusOutcome("s", false, false, false))
	got := get()
	if got.code != http.StatusOK || got.data()["outcome"] != outcomeCompleted {
		t.Fatalf("omitted wait must mean wait:true: code=%d body=%v", got.code, got.body)
	}
}

func TestPromptAndCancelRejectNonPost(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", true))
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec := httptest.NewRecorder()
		handleAgentPromptCentral(rec, httptest.NewRequest(method, "/v1/sessions/s/prompt", nil), h.deps(), "s")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("prompt %s=%d", method, rec.Code)
		}
		rec = httptest.NewRecorder()
		handleAgentCancelCentral(rec, httptest.NewRequest(method, "/v1/sessions/s/cancel", nil), h.deps(), "s")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("cancel %s=%d", method, rec.Code)
		}
	}
}

func TestCancelDeliversAndTakesNoBody(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", true))
	rec := httptest.NewRecorder()
	handleAgentCancelCentral(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/s/cancel", nil), h.deps(), "s")
	got := parseRecorded(t, rec)
	if got.code != http.StatusAccepted || got.data()["admission"] != admissionDelivered {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
	if ep := <-h.cancels; ep != "/tmp/s.sock" {
		t.Fatalf("endpoint %q", ep)
	}
	// An empty object is accepted (a JSON client's natural "no options"),
	// anything meaningful is not.
	rec = httptest.NewRecorder()
	handleAgentCancelCentral(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/s/cancel", strings.NewReader("{}")), h.deps(), "s")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("{} rejected: %s", rec.Body.String())
	}
	<-h.cancels
	rec = httptest.NewRecorder()
	handleAgentCancelCentral(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/s/cancel", strings.NewReader(`{"mode":"steer"}`)), h.deps(), "s")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("body accepted: %d", rec.Code)
	}
	select {
	case ep := <-h.cancels:
		t.Fatalf("invalid cancel delivered to %q", ep)
	default:
	}
}

func TestUnknownSessionIsNotFound(t *testing.T) {
	h := newAgentHarness(t)
	got := runPrompt(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	if got.code != http.StatusNotFound {
		t.Fatalf("code=%d", got.code)
	}
	rec := httptest.NewRecorder()
	handleAgentCancelCentral(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/s/cancel", nil), h.deps(), "s")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cancel code=%d", rec.Code)
	}
}

// ── peer refs ──────────────────────────────────────────────────────────────

// Semantic actions are local-only by design in this slice. A peer-qualified
// ref must be refused, never silently forwarded to somebody else's agent.
func TestPeerRefsAreRefusedNotForwarded(t *testing.T) {
	pm := peering.NewProjectionManager([]config.PeerConfig{{Name: "box", URL: "http://127.0.0.1:1"}}, "self", nil, peering.EventHooks{})
	for _, action := range []string{"prompt", "cancel"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/abc@box/"+action, strings.NewReader(`{"prompt":"hi","mode":"prompt"}`))
		req.Header.Set("Content-Type", "application/json")
		handleCentralSessionAction(rec, req, nil, newSSEFanout(), nil, pm, nil, "", nil)
		got := parseRecorded(t, rec)
		if got.code != http.StatusBadRequest || got.errCode() != codeLocalOnly {
			t.Fatalf("%s: code=%d body=%v", action, got.code, got.body)
		}
	}
	if !agentAction("prompt") || !agentAction("cancel") || agentAction("input") || agentAction("wait") {
		t.Fatal("agentAction must cover exactly the semantic routes")
	}
}

// ── store failures and body limits ──────────────────────────────────────────

// A broken store read is not a missing session: answering 404 would tell the
// caller their session is gone on the strength of a database error.
func TestStoreReadFailureIsInternalNotNotFound(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	got := runPrompt(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	if got.code != http.StatusInternalServerError || got.errCode() != "internal" {
		t.Fatalf("prompt: code=%d body=%v", got.code, got.body)
	}
	rec := httptest.NewRecorder()
	handleAgentCancelCentral(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/s/cancel", nil), h.deps(), "s")
	cancelGot := parseRecorded(t, rec)
	if cancelGot.code != http.StatusInternalServerError || cancelGot.errCode() != "internal" {
		t.Fatalf("cancel: code=%d body=%v", cancelGot.code, cancelGot.body)
	}
	select {
	case ep := <-h.cancels:
		t.Fatalf("delivered despite a failed read: %q", ep)
	default:
	}
}

// Padding must not be able to hide a body: a truncating read would see only
// spaces and accept "no options" while real content followed.
func TestCancelBodyOverflowIsRejected(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", true))
	body := strings.Repeat(" ", 2000) + `{"mode":"steer"}`
	rec := httptest.NewRecorder()
	handleAgentCancelCentral(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/s/cancel", strings.NewReader(body)), h.deps(), "s")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	select {
	case ep := <-h.cancels:
		t.Fatalf("oversized body delivered: %q", ep)
	default:
	}
	// Padding around a legitimately empty object is still fine.
	rec = httptest.NewRecorder()
	handleAgentCancelCentral(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/s/cancel", strings.NewReader("   {}   ")), h.deps(), "s")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("padded {} rejected: %s", rec.Body.String())
	}
}

// ── production wiring ───────────────────────────────────────────────────────

// agentResumeGuard is the real precondition check behind transparent resume: it
// must refuse exactly where POST /resume refuses.
func TestAgentResumeGuard(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	boot := &Bootstrap{Store: st}
	cwd := t.TempDir()
	exited := centralstore.UnixMillis(2)
	live := centralstore.Session{ID: "live", Command: []string{"pi"}, CWD: cwd}
	dead := centralstore.Session{ID: "dead", Command: []string{"pi"}, CWD: cwd, ExitedAt: &exited}
	noCommand := centralstore.Session{ID: "nocmd", CWD: cwd, ExitedAt: &exited}
	goneCwd := centralstore.Session{ID: "gone", Command: []string{"pi"}, CWD: filepath.Join(cwd, "vanished"), ExitedAt: &exited}
	for _, tc := range []struct {
		name     string
		row      centralstore.Session
		gmuxBin  string
		wantCode string
	}{
		{"resumable", dead, "/usr/bin/gmux", ""},
		{"still live row", live, "/usr/bin/gmux", "not_resumable"},
		{"no command", noCommand, "/usr/bin/gmux", "not_resumable"},
		{"no gmux binary", dead, "", "gmux_not_found"},
		// Only reachable with no fallback directory either: ResolveLaunchDir
		// falls back to $HOME, so the guard's cwd_missing verdict means "no
		// usable directory at all".
		{"cwd gone and no fallback", goneCwd, "/usr/bin/gmux", "cwd_missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantCode == "cwd_missing" {
				t.Setenv("HOME", filepath.Join(cwd, "no-such-home"))
			}
			status, code, _ := agentResumeGuard(ctx, boot, tc.gmuxBin, tc.row)
			if code != tc.wantCode {
				t.Fatalf("code=%q status=%d", code, status)
			}
			if (status == 0) != (tc.wantCode == "") {
				t.Fatalf("status %d disagrees with code %q", status, code)
			}
		})
	}
}

// productionSessionID is a well-formed local session ID: the coordinator
// validates it because the ID becomes a filesystem path segment.
const productionSessionID = centralstore.SessionID("1o6lhevd")

// productionIncarnation is the registered runner's ephemeral identity. It is
// not decoration: semantic delivery is conditional on it, so a registration
// without one cannot be prompted at all.
const productionIncarnation = "inc-production"

// productionAgentSetup builds a real store + coordinator + registry with one
// registered runner whose endpoint is a real Unix socket, so the production
// deps (seed subscription, registry liveness, discovery client) are exercised
// rather than modelled.
func productionAgentSetup(t *testing.T, active bool, h http.Handler) (*Bootstrap, string) {
	t.Helper()
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sock := filepath.Join(t.TempDir(), "runner.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	served := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(served) }()
	t.Cleanup(func() { _ = srv.Close(); <-served })

	cwd := t.TempDir()
	facts := centralstore.RunnerFacts{Active: &active, CWD: &cwd}
	runners := &bootstrapRunners{
		metas: map[string]sessioncoord.RunnerMeta{sock: {PID: 4242, Incarnation: productionIncarnation, Registration: centralstore.RunnerRegistration{
			ID: productionSessionID, Adapter: "pi", Alive: true, CreatedAt: 1, ObservedAt: 1, Facts: facts,
		}}},
		blocked: map[string]bool{},
	}
	reg := sessioncoord.NewRegistry()
	coord := sessioncoord.New(reg, runners, st, nil, nil)
	t.Cleanup(coord.Close)
	if _, err := coord.Register(ctx, sessioncoord.RegisterRequest{Endpoint: sock}); err != nil {
		t.Fatal(err)
	}
	return &Bootstrap{Store: st, Registry: reg, Coordinator: coord}, sock
}

// The production subscribe path must hand back a seed that speaks for the live
// generation, because that seed is the admission baseline.
func TestProductionSubscribeSeedIsTheBaseline(t *testing.T) {
	boot, _ := productionAgentSetup(t, true, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	deps := productionAgentDeps(boot, "/usr/bin/gmux")
	runtime, live := deps.live(productionSessionID)
	if !live || runtime.Generation == 0 {
		t.Fatalf("runtime=%+v live=%v", runtime, live)
	}
	seed, _, cancel, err := deps.subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if !seedBaseline(seed, productionSessionID, runtime.Generation) {
		t.Fatal("an active live generation must produce an active baseline")
	}
	// A seed row belonging to another generation is not our baseline...
	if seedBaseline(seed, productionSessionID, runtime.Generation+1) {
		t.Fatal("foreign generation leaked into the baseline")
	}
	// ...and neither is an unknown session.
	if seedBaseline(seed, "other", runtime.Generation) {
		t.Fatal("unknown session must not be active")
	}
	if deps.generationLost(productionSessionID, runtime.Generation) {
		t.Fatal("the installed generation must not read as lost")
	}
	if !deps.generationLost(productionSessionID, runtime.Generation+1) {
		t.Fatal("a superseded generation must read as lost")
	}
}

// One happy path end to end: the real route dispatch, production deps, and a
// real runner socket answering 204.
func TestProductionRoutingDeliversToARealRunnerSocket(t *testing.T) {
	var gotPath, gotExpect string
	var gotBody map[string]any
	boot, _ := productionAgentSetup(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotExpect = r.URL.Path, r.Header.Get("X-Gmux-Expect-Incarnation")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	fanout := newSSEFanout()
	dirs := sessionmeta.New(t.TempDir())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+string(productionSessionID)+"/prompt",
		strings.NewReader(`{"prompt":"stop that","mode":"steer","wait":false}`))
	req.Header.Set("Content-Type", "application/json")
	handleCentralSessionAction(rec, req, boot, fanout, &wire.Converter{}, nil, dirs, "/usr/bin/gmux", nil)
	got := parseRecorded(t, rec)
	if got.code != http.StatusAccepted || got.data()["admission"] != admissionDelivered {
		t.Fatalf("code=%d body=%v", got.code, got.body)
	}
	if gotPath != "/prompt" {
		t.Fatalf("runner path %q", gotPath)
	}
	if gotBody["prompt"] != "stop that" || gotBody["delivery"] != runnerDeliveryNow || gotBody["require"] != runnerRequireActive {
		t.Fatalf("runner body %v", gotBody)
	}
	// The pinned incarnation travels with the request, which is what lets the
	// runner refuse an action decided about its predecessor.
	if gotExpect != productionIncarnation {
		t.Fatalf("expect-incarnation header %q, want %q", gotExpect, productionIncarnation)
	}
	// And cancel over the same wiring.
	rec = httptest.NewRecorder()
	handleCentralSessionAction(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/"+string(productionSessionID)+"/cancel", nil),
		boot, fanout, &wire.Converter{}, nil, dirs, "/usr/bin/gmux", nil)
	if cancelGot := parseRecorded(t, rec); cancelGot.code != http.StatusAccepted {
		t.Fatalf("cancel code=%d body=%v", cancelGot.code, cancelGot.body)
	}
	if gotPath != "/cancel" {
		t.Fatalf("runner path %q", gotPath)
	}
	if gotExpect != productionIncarnation {
		t.Fatalf("cancel expect-incarnation header %q, want %q", gotExpect, productionIncarnation)
	}
}

// TestProductionRoutingMapsARunnersIncarnationRefusal pins the cross-module wire
// literal, over a real runner socket and the production deps.
//
// `incarnation_mismatch` is a string that crosses a module boundary: the runner
// (cli/gmux, ptyserver.CodeIncarnationMismatch) emits it and this daemon
// (discovery.codeIncarnationMismatch) recognizes it. Nothing but agreement on the
// spelling connects them, so the runner here answers with the LITERAL text rather
// than any constant this module owns — renaming either side's constant fails this
// test instead of silently degrading a guaranteed non-delivery into an opaque
// 409 refusal that scripts must treat as indeterminate.
func TestProductionRoutingMapsARunnersIncarnationRefusal(t *testing.T) {
	// The occupant of the pathname is somebody else: it refuses anything not
	// addressed to it, which is exactly what a replacement runner does.
	boot, _ := productionAgentSetup(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gmux-Expect-Incarnation") == "the-runner-that-actually-owns-this-socket" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"incarnation_mismatch","error":"incarnation mismatch: this pathname is owned by a different runner; nothing was delivered"}`))
	}))
	fanout := newSSEFanout()
	dirs := sessionmeta.New(t.TempDir())
	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"prompt", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+string(productionSessionID)+"/prompt",
				strings.NewReader(`{"prompt":"stop that","mode":"steer","wait":false}`))
			r.Header.Set("Content-Type", "application/json")
			return r
		}()},
		{"cancel", httptest.NewRequest(http.MethodPost, "/v1/sessions/"+string(productionSessionID)+"/cancel", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleCentralSessionAction(rec, tc.req, boot, fanout, &wire.Converter{}, nil, dirs, "/usr/bin/gmux", nil)
			got := parseRecorded(t, rec)
			if got.code != http.StatusConflict || got.errCode() != codeIncarnationMismatch {
				t.Fatalf("code=%d body=%v, want 409/%s", got.code, got.body, codeIncarnationMismatch)
			}
			// The public code is the same literal, and the message must promise a
			// safe retry: this is the one delivery failure that is provably a
			// non-delivery.
			if got.errCode() != "incarnation_mismatch" {
				t.Fatalf("public code %q drifted from the wire literal", got.errCode())
			}
			if !strings.Contains(got.errMessage(), "safe to retry") {
				t.Fatalf("message must state the guarantee: %q", got.errMessage())
			}
		})
	}
}

// TestDetachedReturnPointDependsOnWhoStartsTheTurn pins the mode split the CLI's
// `--no-wait` help documents (cli/gmux TestAgentPromptHelpDistinguishesAdmission-
// FromDelivery): whether a detached prompt returns at ADMISSION or at DELIVERY is
// decided by whether it is the thing that starts a turn.
//
// Both halves are contract, and the contrast is what makes each meaningful:
//
//   - a plain prompt starts the turn, so a fresh active edge is observable and is
//     the health event exit 0 buys. It ARMS THE ADMISSION TIMER and does not
//     answer until the edge lands — arming that timer is the observable proof it
//     is waiting for something rather than returning at delivery.
//   - a steer joins a turn that was admitted before this request existed, so
//     there is nothing to admit. It must NOT arm an admission timer and must not
//     wait for an edge, or `--no-wait --steer` would block for the whole window
//     on a session that is behaving perfectly.
//
// A follow-up sits on both sides of the split by design: idle → it starts the
// turn (admission), active → the agent merges it into the running loop
// (delivery), which is why the mode alone cannot decide this.
func TestDetachedReturnPointDependsOnWhoStartsTheTurn(t *testing.T) {
	t.Run("a prompt that starts the turn waits for admission", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{
			"prompt": "hi", "mode": modePrompt, "wait": false,
		}))
		<-h.prompts
		h.nextTimer(t) // the admission window: proof it is waiting for the turn
		h.openTurn(1, "hi")
		h.publish(statusOutcome("s", true, false, false))
		got := get()
		if got.code != http.StatusAccepted || got.data()["admission"] != admissionAccepted {
			t.Fatalf("code=%d body=%v: a plain prompt's exit 0 must mean the turn started", got.code, got.body)
		}
	})

	for _, tc := range []struct {
		name string
		mode string
	}{
		{"a steer joins an admitted turn and returns at delivery", modeSteer},
		{"a follow-up merged into a running turn returns at delivery", modeFollowUp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgentHarness(t, liveRow("s", true)) // a turn is already running
			h.openTurn(9, "the running turn")
			get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{
				"prompt": "and this", "mode": tc.mode, "wait": false,
			}))
			<-h.prompts
			// No edge is ever published: the answer must come anyway.
			got := get()
			if got.code != http.StatusAccepted || got.data()["admission"] != admissionDelivered {
				t.Fatalf("code=%d body=%v: joining a running turn admits nothing beyond delivery", got.code, got.body)
			}
			select {
			case tm := <-h.timers:
				t.Fatalf("armed a %s wait (%s) for a turn that was already admitted", tc.mode, tm.d)
			default:
			}
		})
	}
}
