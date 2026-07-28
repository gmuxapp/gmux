package main

// agent_status.go — `gmux agent status <id>`: the snapshot read.
//
// ADR 0027's 2026-07-28 amendment splits the reading verbs COGNITIVELY rather
// than by output shape:
//
//	gmux tail <id>          the raw screen — what the TUI painted
//	gmux agent logs <id>    the exact text I want (filterable transcript)
//	gmux agent status <id>  I don't know what I want; show me what matters
//
// status therefore has a FIXED three-part skeleton that never varies with
// flags, session state or adapter:
//
//  1. the state line — session identity (short id, adapter), alive/dead,
//     active/idle, the last turn's outcome and a rough recency;
//  2. the triggering excerpt — the first lines of the user message that started
//     the current (or last) turn, under its own heading so it can never be
//     mistaken for the answer;
//  3. the relevant content — the final answer when idle, the last few messages
//     plus a working indicator when active.
//
// It is a SNAPSHOT: store-only, no runner contact, no resume, no persisted
// state. It works on a dead retained session, and it is the one verb allowed to
// read the tape (the conversation) — which is also why its content can be
// staler than a wait's carried result.
//
// The state line is adapter-independent: it comes from the session row. So a
// non-renderer adapter (claude, codex, a shell) gets the state line and a note
// saying its conversation cannot be read, not an error — refusing the whole
// question because part of it is unanswerable would be worse than answering the
// part that is.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// agentStatusTriggerLines caps the trigger excerpt. A prompt can be a whole
// specification; the first lines are what identify it, and status is a summary
// verb — `gmux agent logs --user -n 1 <id>` prints the whole thing.
const agentStatusTriggerLines = 5

// agentStatusRecentMessages is how many conversation messages the active-turn
// content shows. Small on purpose: "what is it doing right now" is answered by
// the last handful, and a longer window is `gmux agent logs`.
const agentStatusRecentMessages = 5

// agentConvMessage is one message of the daemon's JSON conversation format.
type agentConvMessage struct {
	Role  string `json:"role"`
	Type  string `json:"type"`
	Text  string `json:"text"`
	Prose string `json:"prose"`
}

// agentConvRead is a transcript read outcome, with the same three-way split as
// agentAnswerRead: messages, a daemon "nothing to read" code, or a failure.
type agentConvRead struct {
	Messages []agentConvMessage
	Code     string
	Message  string
	Failed   bool
	Report   string
}

// readAgentConversation reads the last n messages of the given types as JSON.
// Store-only, like every read in this namespace.
//
// The types are a parameter rather than a constant because status asks two
// different questions of the same endpoint: the newest USER message (the
// trigger, which must never be crowded out by a busy turn's own tool calls) and
// the last few messages of every type (what the turn is doing). Asking one
// windowed question for both is what let a long turn report "nothing has been
// asked of this session yet" while printing that turn's activity.
func readAgentConversation(sess cliSession, n int, types ...string) agentConvRead {
	client := gmuxdClient()
	// No client deadline, for the reason the other conversation reads drop it:
	// the daemon re-reads and renders the whole stored conversation, which on a
	// long session and a cold filesystem can outlast the default 5s and turn a
	// readable snapshot into a transport error.
	client.Timeout = 0
	url := fmt.Sprintf("%s/v1/sessions/%s/conversation?tail=%d&types=%s&format=json",
		gmuxdBaseURL(), sess.ID, n, strings.Join(types, ","))
	resp, err := client.Get(url)
	if err != nil {
		return agentConvRead{Failed: true, Report: err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		return agentConvRead{Failed: true, Report: err.Error()}
	}
	if resp.StatusCode >= 300 {
		code, msg := errorCode(raw), extractMessage(raw)
		if code == "" {
			return agentConvRead{Failed: true, Report: agentSkewOrRawReport("status", resp.StatusCode, raw)}
		}
		return agentConvRead{Code: code, Message: msg}
	}
	var env struct {
		Data struct {
			Messages []agentConvMessage `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Data.Messages == nil {
		// A 200 this CLI cannot decode is a daemon that does not know the JSON
		// format (it served markdown). Reporting "no messages" would claim the
		// agent has done nothing.
		return agentConvRead{Failed: true, Report: "this gmuxd predates 'gmux agent status' (it cannot serve the conversation as JSON); restart the daemon with 'gmux daemon restart'"}
	}
	return agentConvRead{Messages: env.Data.Messages}
}

// agentStateRow is the slice of the session row the state line needs.
//
// Read as its own shape rather than by widening cliSession: `gmux ls --json` is
// a pinned schema (ADR 0009 13b) and status is not a reason to grow it.
type agentStateRow struct {
	ID     string `json:"id"`
	Status *struct {
		Active      bool `json:"active"`
		Error       bool `json:"error"`
		Interrupted bool `json:"interrupted"`
	} `json:"status"`
	LastOutputAt string `json:"last_output_at"`
	ExitedAt     string `json:"exited_at"`
	StartedAt    string `json:"started_at"`
}

// resolveAgentSessionSnapshot resolves ref AND reads its turn-state row from
// ONE session listing, refusing peer-owned sessions like the rest of the
// namespace.
//
// One listing, not two, for a correctness reason rather than a cost one:
// liveness (`alive`) and the turn flags (`status`) are classified together by
// the state line, and reading them from two snapshots lets a session that dies
// in between be reported as `alive` with its post-death flags — the exact
// liveness/turn-flag pairing this file is careful about everywhere else.
//
// The retry mirrors resolveSession's: a session registered moments ago may not
// be in the composed snapshot yet, and only a clean miss is transient.
func resolveAgentSessionSnapshot(ref, verb string) (cliSession, agentStateRow, bool) {
	const (
		maxRetries = 6
		retryDelay = 100 * time.Millisecond
	)
	for attempt := 0; ; attempt++ {
		sessions, rows, err := fetchSessionsWithRows()
		if err != nil {
			fmt.Fprintln(os.Stderr, "gmux:", err)
			return cliSession{}, agentStateRow{}, false
		}
		sess, err := matchSession(sessions, ref)
		if err != nil {
			if attempt < maxRetries && isNoMatchError(err) {
				time.Sleep(retryDelay)
				continue
			}
			fmt.Fprintln(os.Stderr, "gmux:", err)
			return cliSession{}, agentStateRow{}, false
		}
		if sess.Peer != "" {
			fmt.Fprintf(os.Stderr, "gmux: agent %s is only supported for local sessions (%s is on peer %q); run gmux agent in a session on that host instead\n",
				verb, shortID(sess.ID), sess.Peer)
			return cliSession{}, agentStateRow{}, false
		}
		for _, row := range rows {
			if row.ID == sess.ID {
				return sess, row, true
			}
		}
		// The row came from the same body as the session, so this cannot
		// normally happen; an absent row degrades to "no turn recorded" rather
		// than failing a question the rest of the snapshot can answer.
		return sess, agentStateRow{}, true
	}
}

// fetchSessionsWithRows reads GET /v1/sessions once and decodes the same body
// into both shapes the CLI needs: the pinned cliSession view used for ref
// resolution, and the turn-state row.
func fetchSessionsWithRows() ([]cliSession, []agentStateRow, error) {
	ensureGmuxd()
	client := gmuxdClient()
	resp, err := client.Get(gmuxdBaseURL() + "/v1/sessions")
	if err != nil {
		return nil, nil, fmt.Errorf("contact gmuxd: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("gmuxd returned %s", resp.Status)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("read sessions: %w", err)
	}
	var sessions struct {
		Data []cliSession `json:"data"`
	}
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return nil, nil, fmt.Errorf("decode sessions: %w", err)
	}
	var rows struct {
		Data []agentStateRow `json:"data"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, nil, fmt.Errorf("decode sessions: %w", err)
	}
	return sessions.Data, rows.Data, nil
}

// agentStatusState is part 1 of the report, as data.
//
// Outcome and Cause are omitted rather than emptied when there is no concluded
// turn to describe (a running turn, a session that never reported a status):
// `""` is not in the outcome vocabulary, and a consumer must not have to know
// that it means "ask turn_recorded instead". Recency follows the same rule.
type agentStatusState struct {
	ID      string `json:"id"`
	ShortID string `json:"short_id"`
	Adapter string `json:"adapter"`
	Alive   bool   `json:"alive"`
	Active  bool   `json:"active"`
	Outcome string `json:"last_turn_outcome,omitempty"`
	// Cause qualifies a non-completed outcome the way the daemon's wait verdict
	// does; today the only value is runner_died.
	Cause    string `json:"last_turn_cause,omitempty"`
	Recency  string `json:"last_activity_at,omitempty"`
	Recorded bool   `json:"turn_recorded"`
}

// causeRunnerDied mirrors the daemon's cause vocabulary (agent_actions.go): an
// error outcome caused by the session dying rather than by the agent reporting
// a failed turn.
const causeRunnerDied = "runner_died"

// agentStatusStateOf derives the state line's facts from the session row.
//
// Classification follows the daemon's own rule (wait_pure.go's terminalReason /
// diedConclusion), and it is deliberately keyed on the row's raw Active flag
// BEFORE liveness is considered:
//
//   - a turn still OPEN has not concluded, so no outcome may be attributed to
//     it. On a live session that is "working"; on a DEAD one the open turn will
//     never close, which the daemon calls an error with cause runner_died —
//     never "completed". A runner that dies mid-turn leaves exactly this row
//     (Active=true, Error=false), preserved on purpose by the store's sweep, so
//     ANDing Active away with liveness first and falling through to the default
//     branch made the most ordinary failure the tool has (an agent killed while
//     working) report a turn that succeeded.
//   - a CLOSED turn's outcome is the row's terminal flags: error > interrupted >
//     completed.
//   - a row whose runner never reported a status carries no turn at all, and
//     that is reported as "no turn recorded" rather than guessed as completed —
//     a fresh session has not succeeded at anything.
//
// `gmux wait` and `gmux agent status` must never disagree about what
// "completed" means for one identical row; this is that agreement, spelled out
// on the CLI side because the row is all status has.
func agentStatusStateOf(sess cliSession, row agentStateRow, found bool) agentStatusState {
	st := agentStatusState{
		ID:      sess.ID,
		ShortID: shortID(sess.ID),
		Adapter: sess.Adapter,
		Alive:   sess.Alive,
	}
	if !found || row.Status == nil {
		return st
	}
	st.Recorded = true
	open := row.Status.Active
	// Active means "a turn is running right now", which a dead runner cannot do.
	st.Active = open && sess.Alive
	st.Recency = firstNonEmpty(row.LastOutputAt, row.ExitedAt, row.StartedAt)
	switch {
	case open && sess.Alive:
		// Running: no outcome, in the report and in the JSON alike.
	case open:
		st.Outcome, st.Cause = waitOutcomeError, causeRunnerDied
	case row.Status.Error:
		st.Outcome = waitOutcomeError
	case row.Status.Interrupted:
		st.Outcome = waitOutcomeInterrupted
	default:
		st.Outcome = waitOutcomeCompleted
	}
	return st
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// agentStatusStateLine renders part 1 for humans.
func agentStatusStateLine(st agentStatusState) string {
	liveness := "dead"
	if st.Alive {
		liveness = "alive"
	}
	activity := "idle"
	if st.Active {
		activity = "active"
	}
	adapter := st.Adapter
	if adapter == "" {
		adapter = "unknown adapter"
	}
	line := fmt.Sprintf("%s %s — %s, %s", st.ShortID, adapter, liveness, activity)
	switch {
	case st.Active:
		line += "; working" + agentStatusSince(st.Recency, ", last output ")
	case !st.Recorded:
		line += "; no turn recorded yet"
	case st.Cause == causeRunnerDied:
		// Naming the death beats a bare "error": the turn did not fail, it was
		// never finished, and the answer a caller might be waiting for will
		// never arrive.
		line += "; the turn never finished (runner died)" + agentStatusSince(st.Recency, ", died ")
	default:
		line += "; last turn " + st.Outcome + agentStatusSince(st.Recency, " ")
	}
	return line
}

// agentStatusSince renders a rough recency for an RFC3339 timestamp, or ""
// when there is none to render. Rough on purpose: status answers "recently or
// long ago", and a precise duration would invite arithmetic on a snapshot.
func agentStatusSince(ts, prefix string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return ""
		}
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	var rough string
	switch {
	case d < time.Minute:
		rough = fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		rough = fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		rough = fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		rough = fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return prefix + rough
}

// agentStatusTrigger extracts part 2: the excerpt of the user message that
// started the current or last turn.
//
// The newest user message is the cheapest honest turn boundary available from
// the tape without inventing turn metadata — the same rule the daemon's
// message scope uses. It is an approximation of the runner's asserted trigger,
// and status says so by being a snapshot verb.
func agentStatusTrigger(msgs []agentConvMessage) (text string, truncated bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Type != agentLogTypeUser {
			continue
		}
		lines := strings.Split(strings.TrimRight(msgs[i].Text, "\n"), "\n")
		if len(lines) > agentStatusTriggerLines {
			return strings.Join(lines[:agentStatusTriggerLines], "\n"), true
		}
		return strings.Join(lines, "\n"), false
	}
	return "", false
}

// agentStatusReport is the `--json` shape: one object mirroring the three
// parts, so a script reads the same snapshot the human report shows.
type agentStatusReport struct {
	State   agentStatusState `json:"state"`
	Trigger struct {
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
		// Code/Note are the daemon's reason there is no trigger text, when the
		// reason is that no user message could be READ. An absent code with
		// empty text means the opposite: the read succeeded and the
		// conversation holds no user message, i.e. nothing has been asked. The
		// two facts must stay distinguishable — collapsing them is how a report
		// claims nothing was ever asked of a session whose tape it cannot read.
		Code string `json:"code,omitempty"`
		Note string `json:"note,omitempty"`
	} `json:"trigger"`
	Content struct {
		// Kind is "answer" (idle: the final assistant prose), "recent"
		// (active: the last few messages) or "none" (nothing readable), and it
		// is what a consumer switches on.
		Kind     string             `json:"kind"`
		Text     string             `json:"text,omitempty"`
		Messages []agentConvMessage `json:"messages,omitempty"`
		// Note carries the reason there is no content, as the daemon's stable
		// code plus its prose.
		Code string `json:"code,omitempty"`
		Note string `json:"note,omitempty"`
	} `json:"content"`
}

// Content kinds.
const (
	agentStatusContentAnswer = "answer"
	agentStatusContentRecent = "recent"
	agentStatusContentNone   = "none"
)

// cmdAgentStatus implements `gmux agent status`.
//
// Exit codes: 0 whenever the snapshot could be taken — including for a session
// whose conversation gmux cannot read, because the state line IS an answer to
// the question asked. 1 only when the read itself failed (transport, version
// skew, an unresolvable ref, a peer session).
func cmdAgentStatus(ref string, asJSON bool) int {
	sess, row, ok := resolveAgentSessionSnapshot(ref, "status")
	if !ok {
		return waitExitError
	}
	rep := agentStatusReport{State: agentStatusStateOf(sess, row, true)}

	// The trigger gets its OWN read, filtered to the newest user message. It is
	// the newest user boundary in the whole conversation, not a member of some
	// last-N window: a turn that made thirty tool calls used to push its own
	// prompt out of a shared window, and the report then claimed nothing had
	// ever been asked while printing that turn's activity.
	trigger := readAgentConversation(sess, 1, agentLogTypeUser)
	if trigger.Failed {
		fmt.Fprintln(os.Stderr, "gmux:", trigger.Report)
		return waitExitError
	}
	if trigger.Code != "" {
		// codeNoMessage here means "readable, and no user message in it", which
		// IS "nothing has been asked" — so it stays an absent code with empty
		// text. Any other code means the conversation could not be read, and the
		// report must not turn that into a claim about what was asked.
		if trigger.Code != codeNoMessage {
			rep.Trigger.Code, rep.Trigger.Note = trigger.Code, trigger.Message
		}
	} else {
		rep.Trigger.Text, rep.Trigger.Truncated = agentStatusTrigger(trigger.Messages)
	}

	switch {
	case rep.State.Active:
		conv := readAgentConversation(sess, agentStatusRecentMessages,
			agentLogTypeUser, agentLogTypeAgent, agentLogTypeTool)
		if conv.Failed {
			fmt.Fprintln(os.Stderr, "gmux:", conv.Report)
			return waitExitError
		}
		if conv.Code != "" {
			// Adapter-gated, or nothing recorded yet: the state line stands, and
			// the note says which.
			rep.Content.Kind = agentStatusContentNone
			rep.Content.Code, rep.Content.Note = conv.Code, conv.Message
			break
		}
		rep.Content.Kind = agentStatusContentRecent
		rep.Content.Messages = conv.Messages
	default:
		answer := readAgentAnswer(sess)
		if answer.Failed {
			fmt.Fprintln(os.Stderr, "gmux:", answer.Report)
			return waitExitError
		}
		if answer.Code != "" {
			rep.Content.Kind = agentStatusContentNone
			rep.Content.Code, rep.Content.Note = answer.Code, answer.Message
			break
		}
		rep.Content.Kind = agentStatusContentAnswer
		rep.Content.Text = answer.Text
	}

	if asJSON {
		out, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "gmux:", err)
			return waitExitError
		}
		if _, err := fmt.Fprintln(os.Stdout, string(out)); err != nil {
			fmt.Fprintln(os.Stderr, "gmux:", err)
			return waitExitError
		}
		return waitExitOK
	}
	if _, err := io.WriteString(os.Stdout, renderAgentStatus(rep)); err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	return waitExitOK
}

// renderAgentStatus renders the three-part report.
//
// Every part is announced by a heading, present even when the part is empty:
// the skeleton is the contract, and a reader must never have to guess whether
// the text they are looking at is the prompt or the answer.
func renderAgentStatus(rep agentStatusReport) string {
	var b strings.Builder
	b.WriteString("## State\n\n")
	b.WriteString(agentStatusStateLine(rep.State))
	b.WriteString("\n\n## Triggered by\n\n")
	switch {
	case rep.Trigger.Text == "" && rep.Trigger.Code != "":
		// No user message could be read, so nothing may be claimed about what
		// was asked — only the daemon's reason for not knowing.
		fmt.Fprintf(&b, "(%s: %s)\n", rep.Trigger.Code, orNothingToRead(rep.Trigger.Note))
	case rep.Trigger.Text == "":
		b.WriteString("(nothing has been asked of this session yet)\n")
	default:
		b.WriteString(rep.Trigger.Text)
		b.WriteString("\n")
		if rep.Trigger.Truncated {
			b.WriteString("(excerpt; 'gmux agent logs --user -n 1' prints all of it)\n")
		}
	}
	switch rep.Content.Kind {
	case agentStatusContentAnswer:
		b.WriteString("\n## Answer\n\n")
		b.WriteString(rep.Content.Text)
		b.WriteString("\n")
	case agentStatusContentRecent:
		b.WriteString("\n## Recent\n\n")
		for i, m := range rep.Content.Messages {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "### %s\n\n%s\n", agentStatusRoleHeading(m), strings.TrimRight(m.Text, "\n"))
		}
		b.WriteString("\n(still working — 'gmux wait' blocks until the turn ends)\n")
	default:
		// The third part keeps the name of what it WOULD have held: a running
		// turn has no answer to be missing, it has content that cannot be read.
		if rep.State.Active {
			b.WriteString("\n## Recent\n\n")
		} else {
			b.WriteString("\n## Answer\n\n")
		}
		note := orNothingToRead(rep.Content.Note)
		if rep.Content.Code == codeUnsupportedAdapter || rep.Content.Code == codeUnsupportedAction {
			fmt.Fprintf(&b, "(%s: %s; 'gmux tail %s' shows the terminal instead)\n",
				rep.Content.Code, note, rep.State.ShortID)
			break
		}
		fmt.Fprintf(&b, "(%s: %s)\n", codeOrNothing(rep.Content.Code), note)
	}
	return b.String()
}

func codeOrNothing(code string) string {
	if code == "" {
		return "nothing"
	}
	return code
}

func orNothingToRead(note string) string {
	if note == "" {
		return "nothing to read"
	}
	return note
}

func agentStatusRoleHeading(m agentConvMessage) string {
	switch m.Type {
	case agentLogTypeUser:
		return "User"
	case agentLogTypeTool:
		return "Assistant (tools)"
	default:
		return "Assistant"
	}
}
