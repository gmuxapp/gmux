package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
	"unicode"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// The global gmux exit taxonomy (ADR 0027 §8). It is deliberately small
// and shared by every verb — wait, send --wait, agent — because a
// per-verb code space forces a script that composes them to learn
// several dialects, and the old wait-specific codes (2 = died,
// 3 = timeout) collided with `gmux agent`'s need to report an
// intentional interruption distinctly.
//
//   - waitExitOK (0): success. The turn closed normally, or the output
//     matched --for-text/--for-regex.
//   - waitExitError (1): any error. Usage, transport, unsupported
//     operation, --timeout elapsing, the session dying, and a turn that
//     ended in a terminal failure are the same class of bad news to a
//     caller: what it asked for did not happen. Scripts that need to
//     tell them apart read the stderr line, which names the condition.
//   - waitExitInterrupted (2): the turn was intentionally stopped by a
//     human or another agent. Separate from 1 because it is expected
//     coordination rather than a fault, and it is the one non-success
//     case a caller routinely handles differently.
//
// This is an intentional, pre-release break: there is no 3 any more,
// and 2 no longer means "died".
const (
	waitExitOK          = 0
	waitExitError       = 1
	waitExitInterrupted = 2

	// Leave a small part of the hard invocation deadline for the daemon's
	// timeout response to cross HTTP and be decoded. This is not a grace
	// period: responses at or after the invocation deadline are still rejected.
	waitResponseReserve = 100 * time.Millisecond
)

// exitUsage is the exit code for a command line gmux could not parse. It is
// the error code, named separately only because main.go's parse failure is far
// from this file: under the previous taxonomy it exited 2, which now means
// "intentionally interrupted".
const exitUsage = waitExitError

// Turn conclusions the daemon reports alongside a wait's reason. Same
// vocabulary as the synchronous prompt response — one taxonomy, derived
// once server-side (classifyTurnClose).
const (
	waitOutcomeSnapshot    = "snapshot"
	waitOutcomeCompleted   = "completed"
	waitOutcomeError       = "error"
	waitOutcomeInterrupted = "interrupted"
)

// cmdWait implements `gmux wait <id>... [--quiet] [--timeout N]
// [--for-text S | --for-regex P]`.
//
// The wait itself happens server-side: gmuxd already subscribes to
// per-session events for its own bookkeeping, so we just hand it the
// session id and block on the HTTP response. That keeps the CLI free
// of SSE-parsing logic and ensures the idle-detection rules (how turn
// state resolves, what counts as "died") live in one place. Output conditions equally belong server-side: the bytes
// live in the daemon's scrollback tee, and matching there can't miss
// output the way client-side scrollback polling could.
//
// Agent waits are exchange-bearing: the daemon relays the observed source
// frame and the CLI renders it on stdout for every domain outcome. --quiet
// suppresses that document. Predicate waits and non-agent process sessions
// remain synchronization-only.
//
// Local sessions only: the daemon's wait handler resolves the session
// against its local store and consults the adapter allowlist; remote
// peer sessions are out of scope until peer subscriptions stream
// Status events back to the hub.
func cmdWait(refs []string, timeoutSecs int, forText, forRegex string, quiet bool) int {
	// Start the one invocation deadline before resolution. Resolution remains
	// all-or-none, but its requests and read-your-writes retries consume the same
	// budget as the waits they precede.
	ctx := context.Background()
	cancel := func() {}
	if timeoutSecs > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	}
	defer cancel()

	// Resolve the complete set before issuing any wait request. Besides making
	// unknown and ambiguous refs fail fast, this is what makes duplicate
	// detection meaningful for aliases and prefixes that name the same session.
	sessions := make([]cliSession, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for i, ref := range refs {
		sess, err := resolveSessionContext(ctx, ref)
		if err != nil {
			if timeoutSecs > 0 && (errors.Is(err, context.DeadlineExceeded) || waitInvocationExpired(ctx)) {
				reportWaitInvocationTimeout(timeoutSecs)
			} else {
				fmt.Fprintln(os.Stderr, "gmux:", err)
			}
			return waitExitError
		}
		if sess.Peer != "" {
			fmt.Fprintf(os.Stderr, "gmux: wait is only supported for local sessions (%s is on peer %q)\n",
				sess.ID, sess.Peer)
			return waitExitError
		}
		if _, duplicate := seen[sess.ID]; duplicate {
			fmt.Fprintf(os.Stderr, "gmux: wait: duplicate session %s\n", sess.ID)
			return waitExitError
		}
		seen[sess.ID] = struct{}{}
		sessions[i] = sess
	}

	type result struct {
		code   int
		report bytes.Buffer
	}
	results := make([]result, len(sessions))
	serverTimeout := time.Duration(0)
	if timeoutSecs > 0 {
		// Resolution is inside the hard whole-call deadline. Hand the daemon the
		// precise budget that remains, less a bounded response/transport reserve,
		// so its authoritative partial timeout report normally arrives first.
		deadline, ok := ctx.Deadline()
		remaining := time.Until(deadline)
		if ok && remaining > waitResponseReserve {
			serverTimeout = (remaining - waitResponseReserve).Truncate(time.Millisecond)
		}
	}

	agentWait := true
	for _, sess := range sessions {
		agentWait = agentWait && isAgentSession(sess)
	}
	stopNotice, signalObserved := observeInterruptedWait(os.Stdout, quiet, agentWait)
	var wg sync.WaitGroup
	wg.Add(len(sessions))
	for i := range sessions {
		go func() {
			defer wg.Done()
			results[i].code = waitSession(ctx, sessions[i], timeoutSecs, serverTimeout, forText, forRegex, quiet, &results[i].report)
		}()
	}
	wg.Wait()
	stopNotice()
	select {
	case sig := <-signalObserved:
		// The notice is already the complete stdout contract. Do not flush
		// reports that happened to settle during dieFromSignal's fallback delay.
		return exitSignaled(sig)
	default:
	}

	if !quiet {
		multi := len(sessions) > 1
		for i := range sessions {
			if multi {
				if i > 0 {
					fmt.Fprintln(os.Stdout)
				}
				fmt.Fprintf(os.Stdout, "=== %s ===\n\n", sessions[i].ID)
			}
			_, _ = results[i].report.WriteTo(os.Stdout)
		}
	}

	// Failure and timeout dominate interruption; interruption dominates clean
	// completion. This is independent of settlement and argv order.
	exit := waitExitOK
	for i := range results {
		if results[i].code == waitExitError {
			return waitExitError
		}
		if results[i].code == waitExitInterrupted {
			exit = waitExitInterrupted
		}
	}
	return exit
}

// waitSession performs one already-resolved wait. Resolution and signal
// handling belong to cmdWait so a multi-wait arms all-or-none and installs one
// process-wide signal notice.
func waitSession(ctx context.Context, sess cliSession, timeoutSecs int, serverTimeout time.Duration, forText, forRegex string, quiet bool, stdout io.Writer) int {
	predicate := forText != "" || forRegex != ""
	agent := isAgentSession(sess)
	if timeoutSecs > 0 && serverTimeout < time.Millisecond {
		return reportLocalWaitTimeout(predicate, quiet, timeoutSecs, stdout)
	}

	query := url.Values{}
	if timeoutSecs > 0 {
		query.Set("timeout_ms", strconv.FormatInt(serverTimeout.Milliseconds(), 10))
	}
	if forText != "" {
		query.Set("for_text", forText)
	}
	if forRegex != "" {
		query.Set("for_regex", forRegex)
	}
	endpoint := gmuxdBaseURL() + "/v1/sessions/" + url.PathEscape(sess.ID) + "/wait"
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	client := gmuxdClient()
	// The default 5s client timeout would cut off an unbounded wait on a slow
	// agent. With it disabled, the shared invocation context is the only local
	// deadline; the timeout query also lets the daemon return a detailed report.
	client.Timeout = 0

	// No request body; pass http.NoBody so we don't advertise a
	// content-type for bytes that don't exist. The shared context is a hard
	// whole-group deadline; the daemon timeout normally wins and supplies the
	// partial exchange report, while this catches delayed or stalled requests.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	resp, err := client.Do(req)
	if err != nil {
		if timeoutSecs > 0 && (errors.Is(err, context.DeadlineExceeded) || waitInvocationExpired(ctx)) {
			return reportLocalWaitTimeout(forText != "" || forRegex != "", quiet, timeoutSecs, stdout)
		}
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	defer resp.Body.Close()

	if timeoutSecs > 0 && waitInvocationExpired(ctx) {
		return reportLocalWaitTimeout(predicate, quiet, timeoutSecs, stdout)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var env struct {
			Data waitResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			if timeoutSecs > 0 && (errors.Is(err, context.DeadlineExceeded) || waitInvocationExpired(ctx)) {
				return reportLocalWaitTimeout(predicate, quiet, timeoutSecs, stdout)
			}
			fmt.Fprintln(os.Stderr, "gmux: decode wait response:", err)
			return waitExitError
		}
		if timeoutSecs > 0 && waitInvocationExpired(ctx) {
			return reportLocalWaitTimeout(predicate, quiet, timeoutSecs, stdout)
		}
		code := reportWaitResult(env.Data, predicate, quiet, agent, stdout)
		if code == waitExitOK {
			if err := consumeSession(sess, env.Data.UnreadToken); err != nil {
				fmt.Fprintf(os.Stderr, "gmux: wait could not mark %s read: %v\n", displayID(sess), err)
				return waitExitError
			}
		}
		return code
	case http.StatusRequestTimeout:
		if predicate {
			fmt.Fprintf(os.Stderr, "gmux: the session's output did not match within %ds\n", timeoutSecs)
			return waitExitError
		}
		var env struct {
			Data waitResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			if timeoutSecs > 0 && (errors.Is(err, context.DeadlineExceeded) || waitInvocationExpired(ctx)) {
				return reportLocalWaitTimeout(false, quiet, timeoutSecs, stdout)
			}
			fmt.Fprintln(os.Stderr, "gmux: decode timeout report:", err)
			return waitExitError
		}
		if timeoutSecs > 0 && waitInvocationExpired(ctx) {
			return reportLocalWaitTimeout(false, quiet, timeoutSecs, stdout)
		}
		return renderWait(env.Data, quiet, timeoutSecs, "", agent, stdout)
	case http.StatusUnprocessableEntity:
		// Current daemons only send 422 on the send --wait path
		// (input_no_submit); older daemons also rejected sessions
		// without an idle signal here. Surface the daemon's message
		// either way — it explains what to change.
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "gmux: wait not supported for this session: %s\n",
			extractMessage(body))
		return waitExitError
	case http.StatusNotFound:
		// Means the session id is unknown to gmuxd entirely.
		fmt.Fprintf(os.Stderr, "gmux: session %s not found\n", displayID(sess))
		return waitExitError
	default:
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "gmux: wait failed: %s: %s\n", resp.Status, extractMessage(body))
		return waitExitError
	}
}

func waitInvocationExpired(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && !time.Now().Before(deadline)
}

func reportWaitInvocationTimeout(timeoutSecs int) {
	fmt.Fprintf(os.Stderr, "gmux: wait timed out after %ds before any session was armed\n", timeoutSecs)
}

// reportLocalWaitTimeout is the hard-deadline fallback for a daemon that did
// not return its authoritative timeout frame. Do not invent exchange history,
// activity state, or iteration counts the client never received.
func reportLocalWaitTimeout(predicate, quiet bool, timeoutSecs int, stdout io.Writer) int {
	if predicate {
		fmt.Fprintf(os.Stderr, "gmux: the session's output did not match within %ds\n", timeoutSecs)
	} else if !quiet {
		fmt.Fprintf(stdout, "[Wait timed out after %ds; session state unknown]\n", timeoutSecs)
	}
	return waitExitError
}

// waitResult is the daemon's resolved-wait payload.
//
// Reason is the synchronization fact ("idle", "matched", "died");
// Outcome is the observed conclusion or explicit snapshot discriminator.
// Output is source-asserted terminal assistant prose; for interrupted, failed
// or timed-out work the shared renderer labels it partial.
type waitResult struct {
	Reason           string             `json:"reason"`
	Exchanges        []adapter.Exchange `json:"exchanges"`
	OmittedExchanges int                `json:"omitted_exchanges"`
	OmittedBytes     int                `json:"omitted_bytes"`
	Previous         *int               `json:"previous_exchanges"`
	AnchorOrdinal    uint64             `json:"anchor_ordinal"`
	BaselineOrdinal  uint64             `json:"baseline_ordinal"`
	Outcome          string             `json:"outcome"`
	Cause            string             `json:"cause"`
	Diagnostic       string             `json:"diagnostic"`
	Trigger          string             `json:"trigger"` // compatibility with pre-exchange runner frames
	Output           string             `json:"output"`
	TerminalPartial  bool               `json:"terminal_partial"`
	UnreadToken      string             `json:"unread_token"`
	// Truncated says the adapter capped Output at the source. stdout still
	// carries what there is (silently dropping the tail would be worse), and the
	// fact goes to stderr where the account belongs.
	Truncated bool `json:"truncated"`
}

// reportWaitResult turns a resolved wait into output and an exit code.
//
// verb names the command being reported so a diagnostic can be honest about
// which one hit a limitation: `send --wait` shares every exit decision here but
// is deliberately result-free, so telling its caller the daemon "predates
// result-bearing waits" would describe a feature they did not ask for.
func reportWaitResult(res waitResult, predicate, quiet, agent bool, stdout io.Writer) int {
	switch res.Reason {
	case "matched":
		// Predicate waits are synchronization-only; matched bytes remain in the
		// terminal stream and are not an agent exchange result.
		return waitExitOK
	case "died":
		if predicate {
			fmt.Fprintln(os.Stderr, "gmux: the session exited before its output matched")
			return waitExitError
		}
		return renderWait(res, quiet, 0, "", agent, stdout)
	case "idle":
		if predicate {
			// A predicate wait resolves on "matched" or "died" only;
			// "idle" would mean the daemon answered a different question.
			fmt.Fprintf(os.Stderr, "gmux: unexpected wait reason %q for an output condition\n", res.Reason)
			return waitExitError
		}
		return reportTurnConclusion(res, quiet, agent, stdout)
	default:
		fmt.Fprintf(os.Stderr, "gmux: unexpected wait reason %q\n", res.Reason)
		return waitExitError
	}
}

// reportTurnConclusion renders a closed turn's conclusion.
//
// A missing outcome is version skew, not a completion: a daemon that
// predates turn conclusions always resolves a closed turn as bare "idle", and
// silently treating that as success would report a failed or interrupted turn as
// a clean one — under exit 0, with no result. Fail loudly and name the fix.
func reportTurnConclusion(res waitResult, quiet, agent bool, stdout io.Writer) int {
	return renderWait(res, quiet, 0, "", agent, stdout)
}

func isAgentSession(sess cliSession) bool {
	_, ok := adapters.FindByAdapter(sess.Adapter).(adapter.ConversationExchangeRenderer)
	return ok
}

func renderWait(res waitResult, quiet bool, timeoutSecs int, submitted string, agent bool, stdout io.Writer) int {
	if agent {
		return renderExchangeWait(res, quiet, timeoutSecs, submitted, stdout)
	}
	if res.Outcome == "" {
		fmt.Fprintln(os.Stderr, "gmux: daemon response has no turn outcome; restart gmuxd to resolve version skew")
		return waitExitError
	}
	exit := waitExitOK
	marker := ""
	switch res.Outcome {
	case waitOutcomeSnapshot:
		marker = "[Session inactive]"
	case waitOutcomeCompleted:
		marker = "[Session activity completed]"
	case waitOutcomeInterrupted:
		marker, exit = "[Session activity interrupted]", waitExitInterrupted
	case waitOutcomeError:
		marker, exit = "[Session activity failed: "+failureReason(res, false)+"]", waitExitError
	case outcomeTimeout:
		marker, exit = fmt.Sprintf("[Wait timed out after %ds; session remains active]", timeoutSecs), waitExitError
	default:
		fmt.Fprintf(os.Stderr, "gmux: unexpected turn outcome %q\n", res.Outcome)
		return waitExitError
	}
	if !quiet {
		fmt.Fprintln(stdout, marker)
	}
	return exit
}

func renderExchangeWait(res waitResult, quiet bool, timeoutSecs int, submitted string, stdout io.Writer) int {
	if res.Outcome == "" {
		fmt.Fprintln(os.Stderr, "gmux: daemon response has no turn outcome; restart gmuxd to resolve version skew")
		return waitExitError
	}
	outcome := adapter.ExchangeSnapshot
	exit := waitExitOK
	switch res.Outcome {
	case waitOutcomeSnapshot:
		outcome = adapter.ExchangeSnapshot
	case waitOutcomeCompleted:
		outcome = adapter.ExchangeCompleted
	case waitOutcomeInterrupted:
		outcome, exit = adapter.ExchangeInterrupted, waitExitInterrupted
	case waitOutcomeError:
		outcome, exit = adapter.ExchangeFailed, waitExitError
	case outcomeTimeout:
		outcome, exit = adapter.ExchangeTimeout, waitExitError
	default:
		fmt.Fprintf(os.Stderr, "gmux: unexpected turn outcome %q\n", res.Outcome)
		return waitExitError
	}
	exchanges := append([]adapter.Exchange(nil), res.Exchanges...)
	unscopedTerminal := ""
	if len(exchanges) == 0 && res.Output != "" {
		if res.Trigger != "" {
			exchanges = append(exchanges, adapter.Exchange{User: res.Trigger, Terminal: res.Output})
		} else {
			unscopedTerminal = res.Output
		}
	}
	if len(exchanges) > 0 {
		if res.Output != "" {
			exchanges[len(exchanges)-1].Terminal = res.Output
		}
		for i := range exchanges {
			anchor := res.AnchorOrdinal != 0 && exchanges[i].Ordinal == res.AnchorOrdinal
			postBaselineMatch := submitted != "" && exchanges[i].User == submitted && exchanges[i].Ordinal > res.BaselineOrdinal
			if anchor || postBaselineMatch {
				exchanges[i].User = abbreviateUser(exchanges[i].User)
			}
		}
	}
	if !quiet {
		_, _ = stdout.Write(adapter.RenderExchangeReport(adapter.ExchangeReport{
			Exchanges: exchanges, Outcome: outcome, Diagnostic: failureReason(res, true), UnscopedTerminal: unscopedTerminal,
			TerminalPartial:   res.TerminalPartial || (res.Output != "" && outcome != adapter.ExchangeCompleted && outcome != adapter.ExchangeSnapshot),
			TerminalTruncated: res.Truncated, TimeoutSeconds: timeoutSecs,
			OmittedExchanges: res.OmittedExchanges, OmittedBytes: res.OmittedBytes,
			PreviousKnown: res.Previous != nil, Previous: valueOrZero(res.Previous),
		}))
	}
	return exit
}

func valueOrZero(v *int) int {
	if v != nil {
		return *v
	}
	return 0
}

func failureReason(res waitResult, agent bool) string {
	if res.Diagnostic != "" {
		return res.Diagnostic
	}
	if res.Cause == causeRunnerDied {
		if agent {
			return "agent activity was lost"
		}
		return "session process exited before the activity completed"
	}
	if res.Cause != "" {
		return res.Cause
	}
	return "activity could not be completed"
}

func abbreviateUser(s string) string {
	runes := []rune(s)
	cut := len(runes)
	if cut > 240 {
		cut = 240
	}
	words := 0
	inWord := false
	for i, r := range runes[:cut] {
		space := unicode.IsSpace(r)
		if !space && !inWord {
			words++
			if words == 21 {
				cut = i
				break
			}
		}
		inWord = !space
	}
	if cut < len(runes) {
		return string(runes[:cut]) + "…"
	}
	return s
}

const (
	causeRunnerDied = "runner_died"
	outcomeTimeout  = "timeout"
)

// extractMessage pulls the .error.message field out of gmuxd's
// standard error envelope, falling back to the raw body if the
// shape doesn't match.
func extractMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return string(body)
}
