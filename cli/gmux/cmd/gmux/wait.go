package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
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
	waitOutcomeCompleted   = "completed"
	waitOutcomeError       = "error"
	waitOutcomeInterrupted = "interrupted"
)

// cmdWait implements `gmux wait <id> [--quiet] [--timeout N]
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
// A wait is also conditionally RESULT-BEARING (ADR 0027 §11 and its
// "where a result-bearing answer comes from" amendment): when a
// renderer-capable agent session completes its turn normally, the
// daemon returns the same latest-final-message `gmux agent output`
// selects and this prints it on stdout. Deliberate omissions:
//
//   - --quiet suppresses it (pure synchronization);
//   - an error, an interruption or a death prints NO result. The newest
//     stored message would belong to a previous or partial turn, and
//     presenting it as this turn's answer is worse than silence; the
//     condition goes to stderr instead. `gmux agent output` remains
//     available for explicit inspection;
//   - predicate waits (--for-text/--for-regex) and shell/process
//     sessions stay synchronization-only, exactly as before: the daemon
//     sends no output for them.
//
// Local sessions only: the daemon's wait handler resolves the session
// against its local store and consults the adapter allowlist; remote
// peer sessions are out of scope until peer subscriptions stream
// Status events back to the hub.
func cmdWait(ref string, timeoutSecs int, forText, forRegex string, quiet bool) int {
	sess, err := resolveSession(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	if sess.Peer != "" {
		// Use the bare shortID here: the message already names the peer
		// separately, so displayID's "shortID@peer" would just repeat it.
		fmt.Fprintf(os.Stderr, "gmux: wait is only supported for local sessions (%s is on peer %q)\n",
			shortID(sess.ID), sess.Peer)
		return waitExitError
	}

	query := url.Values{}
	if timeoutSecs > 0 {
		query.Set("timeout", strconv.Itoa(timeoutSecs))
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
	// The default 5s client timeout would cut off any wait that
	// outlasts a turn on a slow agent. With no client-side timeout
	// the only deadline is the optional server-side --timeout.
	client.Timeout = 0

	// No request body; pass http.NoBody so we don't advertise a
	// content-type for bytes that don't exist.
	// A blocking wait says what a ^C does and does not mean: only the wait stops.
	// The session and its turn keep running, and re-arming is one command away —
	// without the notice, the interrupt reads like the agent was stopped.
	//
	// Installed around the blocking call ONLY, and torn down the moment the
	// response is in hand: a notice printed after the wait already resolved (and
	// printed its answer) would be a lie, and that window is reachable — a ^C
	// pressed just as the turn ended lands there.
	stopNotice := noticeInterruptedWait(os.Stderr, sess.ID)
	resp, err := client.Post(endpoint, "", http.NoBody)
	stopNotice()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	defer resp.Body.Close()

	predicate := forText != "" || forRegex != ""
	switch resp.StatusCode {
	case http.StatusOK:
		var env struct {
			Data waitResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			fmt.Fprintln(os.Stderr, "gmux: decode wait response:", err)
			return waitExitError
		}
		return reportWaitResult(sess, "gmux wait", env.Data, predicate, quiet, os.Stdout)
	case http.StatusRequestTimeout:
		fmt.Fprintf(os.Stderr, "gmux: wait timed out after %ds\n", timeoutSecs)
		return waitExitError
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

// waitResult is the daemon's resolved-wait payload.
//
// Reason is the synchronization fact ("idle", "matched", "died");
// Outcome is the turn's conclusion, present whenever a turn boundary was
// observed; Output carries the agent's answer and is present only for a
// completed turn on a renderer-capable session.
type waitResult struct {
	Reason  string `json:"reason"`
	Outcome string `json:"outcome"`
	Cause   string `json:"cause"`
	Output  string `json:"output"`
	// Truncated says the adapter capped Output at the source. stdout still
	// carries what there is (silently dropping the tail would be worse), and the
	// fact goes to stderr where the account belongs.
	Truncated bool `json:"truncated"`
	// ConversationReadable says whether `gmux agent output` can answer for
	// this session at all. Shells, one-shot commands and Claude/Codex
	// sessions have no semantic conversation, and pointing them at that verb
	// would send the caller to a route that answers 404.
	ConversationReadable bool `json:"conversation_readable"`
}

// reportWaitResult turns a resolved wait into output and an exit code.
//
// verb names the command being reported so a diagnostic can be honest about
// which one hit a limitation: `send --wait` shares every exit decision here but
// is deliberately result-free, so telling its caller the daemon "predates
// result-bearing waits" would describe a feature they did not ask for.
func reportWaitResult(sess cliSession, verb string, res waitResult, predicate, quiet bool, stdout io.Writer) int {
	switch res.Reason {
	case "matched":
		// Predicate wait: synchronization only, by design. The matched
		// bytes are terminal output the caller can read with gmux tail;
		// they are not an agent result.
		return waitExitOK
	case "died":
		if predicate {
			fmt.Fprintf(os.Stderr, "gmux: session %s exited before its output matched\n", displayID(sess))
		} else {
			fmt.Fprintf(os.Stderr, "gmux: session %s died before its turn ended\n", displayID(sess))
		}
		return waitExitError
	case "idle":
		if predicate {
			// A predicate wait resolves on "matched" or "died" only;
			// "idle" would mean the daemon answered a different question.
			fmt.Fprintf(os.Stderr, "gmux: unexpected wait reason %q for an output condition\n", res.Reason)
			return waitExitError
		}
		return reportTurnConclusion(sess, verb, res, quiet, stdout)
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
func reportTurnConclusion(sess cliSession, verb string, res waitResult, quiet bool, stdout io.Writer) int {
	switch res.Outcome {
	case "":
		fmt.Fprintf(os.Stderr, "gmux: this gmuxd predates the turn conclusions '%s' needs (it reported no turn outcome); restart the daemon with 'gmux daemon restart'\n", verb)
		return waitExitError
	case waitOutcomeCompleted:
		if quiet || res.Output == "" {
			// Nothing to print: --quiet, a shell/process session, or an
			// agent whose conversation gmux cannot read. Silence here is
			// the pre-existing synchronization behavior, not a failure.
			return waitExitOK
		}
		// Verbatim, with exactly one trailing newline so the output is usable
		// in a shell without swallowing a final one.
		if _, err := io.WriteString(stdout, strings.TrimRight(res.Output, "\n")+"\n"); err != nil {
			fmt.Fprintln(os.Stderr, "gmux:", err)
			return waitExitError
		}
		noteTruncatedAnswer(sess, res.Truncated)
		return waitExitOK
	case waitOutcomeInterrupted:
		fmt.Fprintf(os.Stderr, "gmux: the turn was interrupted before it finished (session %s); %s\n",
			displayID(sess), inspectHint(sess, res.ConversationReadable))
		return waitExitInterrupted
	case waitOutcomeError:
		if res.Cause != "" {
			fmt.Fprintf(os.Stderr, "gmux: the turn ended in an error (%s); %s\n",
				res.Cause, inspectHint(sess, res.ConversationReadable))
		} else {
			fmt.Fprintf(os.Stderr, "gmux: the turn ended in an error (session %s); %s\n",
				displayID(sess), inspectHint(sess, res.ConversationReadable))
		}
		return waitExitError
	default:
		fmt.Fprintf(os.Stderr, "gmux: unexpected turn outcome %q\n", res.Outcome)
		return waitExitError
	}
}

// noteTruncatedAnswer tells the caller on stderr that the answer they just got
// on stdout is not the whole one. The adapter caps a turn's output at the source
// so an enormous answer can never cost the turn's close; the full text is still
// in the conversation, which is what the hint points at.
func noteTruncatedAnswer(sess cliSession, truncated bool) {
	if !truncated {
		return
	}
	fmt.Fprintf(os.Stderr, "gmux: the answer was truncated at the agent; read it in full with 'gmux agent output %s'\n",
		shortID(sess.ID))
}

// inspectHint names the verb that can actually show what happened.
//
// `gmux agent output` only exists for sessions with a readable agent
// conversation. A failed one-shot command (`gmux -d -- make build`, whose
// non-zero exit closes its lifetime turn with Error=true) or a Claude/Codex
// session has none, and sending the caller there would answer 404 — for those,
// the terminal output is the record.
func inspectHint(sess cliSession, conversationReadable bool) string {
	if conversationReadable {
		return "read what exists with 'gmux agent output " + shortID(sess.ID) + "'"
	}
	return "see its output with 'gmux tail " + shortID(sess.ID) + "'"
}

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
