package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestParseAgentLogsFilters pins the filter grammar: type flags REPLACE the
// default set, they compose, --all is the whole set and cannot be narrowed, and
// --thinking is refused by name rather than answering with silence.
func TestParseAgentLogsFilters(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want []string
	}{
		{[]string{"agent", "logs", "s1"}, nil}, // default: the daemon's pair
		{[]string{"agent", "logs", "--user", "s1"}, []string{"user"}},
		{[]string{"agent", "logs", "--agent", "s1"}, []string{"agent"}},
		{[]string{"agent", "logs", "--tool", "s1"}, []string{"tool"}},
		{[]string{"agent", "logs", "--user", "--tool", "s1"}, []string{"user", "tool"}},
		{[]string{"agent", "logs", "s1", "--all"}, []string{"user", "agent", "tool"}},
	} {
		c, err := parseCLI(tt.args)
		if err != nil {
			t.Fatalf("parseCLI(%v): %v", tt.args, err)
		}
		if strings.Join(c.logTypes, ",") != strings.Join(tt.want, ",") {
			t.Errorf("parseCLI(%v) types = %v, want %v", tt.args, c.logTypes, tt.want)
		}
	}

	// --json is a serialization, not a type: it leaves the filter alone.
	c, err := parseCLI([]string{"agent", "logs", "--json", "--agent", "-n", "1", "s1"})
	if err != nil || !c.json || strings.Join(c.logTypes, ",") != "agent" || c.tailLines != 1 {
		t.Fatalf("parseCLI(--json --agent -n 1) = (%+v, %v)", c, err)
	}

	for _, tt := range []struct {
		name, want string
		args       []string
	}{
		{"thinking is refused by name", "not rendered by this adapter",
			[]string{"agent", "logs", "--thinking", "s1"}},
		{"all cannot be narrowed", "cannot be combined with a type flag",
			[]string{"agent", "logs", "--all", "--user", "s1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCLI(tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("parseCLI(%v) = %v, want an error mentioning %q", tt.args, err, tt.want)
			}
		})
	}
}

// TestAgentLogsRequestCarriesTheFilter: the filter and the serialization travel
// to the daemon, so -n counts POST-filter messages and the daemon serves
// exactly what was asked for. Filtering client-side would make -n mean "of the
// last N messages, the ones that matched", which is a different question.
func TestAgentLogsRequestCarriesTheFilter(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("## User\n\nhi\n")) })
	captureStdout(t, func() {
		if code := cmdAgentLogs("abcd1234", 1, []string{"agent"}, false); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	q, err := url.ParseQuery(d.lastRequest(t).query)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if q.Get("tail") != "1" || q.Get("types") != "agent" || q.Get("format") != "" {
		t.Errorf("query = %v, want tail=1&types=agent and no format", q)
	}

	// The default filter is the daemon's default: no types parameter at all, so
	// there is one definition of "the conversation" rather than two.
	captureStdout(t, func() { cmdAgentLogs("abcd1234", 100, nil, false) })
	q, _ = url.ParseQuery(d.lastRequest(t).query)
	if _, present := q["types"]; present {
		t.Errorf("query = %v, want no types parameter for the default view", q)
	}
}

// TestAgentLogsJSONPrintsABareArray: the machine contract is the array, not the
// daemon's envelope, and an envelope this CLI cannot decode is version skew
// rather than an empty conversation.
func TestAgentLogsJSONPrintsABareArray(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{"messages":[{"role":"assistant","type":"agent","text":"done","prose":"done"}]}}`))
	})
	stdout := captureStdout(t, func() {
		if code := cmdAgentLogs("abcd1234", 1, []string{"agent"}, true); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	var msgs []map[string]any
	if err := json.Unmarshal([]byte(stdout), &msgs); err != nil {
		t.Fatalf("stdout is not a JSON array: %v (%q)", err, stdout)
	}
	if len(msgs) != 1 || msgs[0]["role"] != "assistant" || msgs[0]["text"] != "done" {
		t.Errorf("stdout = %q, want one {role,text,...} object", stdout)
	}
	q, _ := url.ParseQuery(d.lastRequest(t).query)
	if q.Get("format") != "json" {
		t.Errorf("query = %v, want format=json", q)
	}

	// A markdown-serving daemon (no JSON support) must not look like an empty
	// conversation.
	d.on(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("## User\n\nhi\n")) })
	stderr := captureStderr(t, func() {
		out := captureStdout(t, func() {
			if code := cmdAgentLogs("abcd1234", 1, nil, true); code != waitExitError {
				t.Errorf("skew: exit = %d, want 1", code)
			}
		})
		if out != "" {
			t.Errorf("skew printed %q", out)
		}
	})
	if !strings.Contains(stderr, "restart the daemon") {
		t.Errorf("skew stderr = %q", stderr)
	}
}

// statusStub wires a daemon stub that answers both reads `agent status`
// performs: the JSON transcript and the message scope.
func statusStub(t *testing.T, rows []map[string]any, messages string, answer string, answerCode string) *stubDaemon {
	t.Helper()
	d := startStubDaemon(t, localSession())
	d.mu.Lock()
	d.rows = rows
	d.mu.Unlock()
	d.on(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") == conversationScopeMessage {
			if answerCode != "" {
				writeErrEnvelope(w, http.StatusNotFound, answerCode, "the agent has not produced a message yet")
				return
			}
			w.Header().Set(conversationScopeHeader, conversationScopeMessage)
			_, _ = w.Write([]byte(answer + "\n"))
			return
		}
		_, _ = w.Write([]byte(messages))
	})
	return d
}

func piRow(active bool, extra map[string]any) []map[string]any {
	return piRowAlive(true, map[string]any{"active": active}, extra)
}

// piRowAlive builds a session row with an explicit liveness and an explicit
// status map, so the liveness/turn-flag COMBINATIONS can be pinned — the
// dead-and-active row (a runner that died mid-turn) above all.
func piRowAlive(alive bool, status map[string]any, extra map[string]any) []map[string]any {
	row := map[string]any{
		"id":      "sess-abcd1234",
		"adapter": "pi",
		"alive":   alive,
		"status":  status,
	}
	for k, v := range extra {
		row[k] = v
	}
	return []map[string]any{row}
}

const statusMessagesJSON = `{"ok":true,"data":{"messages":[
	{"role":"user","type":"user","text":"run the tests\nand report\nline3\nline4\nline5\nline6","prose":"run the tests"},
	{"role":"assistant","type":"tool","text":"[tool] bash {\"cmd\":\"go test\"}"},
	{"role":"assistant","type":"agent","text":"All green.","prose":"All green."}
]}}`

// TestAgentStatusIdleShape pins the fixed three-part skeleton on an idle
// session: state line, labeled trigger excerpt, then the final answer. The
// labels are the contract — a reader must never have to guess whether the text
// in front of them is the prompt or the answer.
func TestAgentStatusIdleShape(t *testing.T) {
	statusStub(t, piRow(false, map[string]any{"last_output_at": "2020-01-01T00:00:00Z"}), statusMessagesJSON, "All green.", "")
	stdout := captureStdout(t, func() {
		if code := cmdAgentStatus("abcd1234", false); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	for _, want := range []string{
		"## State", "abcd1234 pi — alive, idle; last turn completed",
		"## Triggered by", "run the tests",
		"## Answer", "All green.",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output missing %q:\n%s", want, stdout)
		}
	}
	// Order matters: the trigger must never be printed after the answer, where
	// a skimming reader would read it as part of it.
	if strings.Index(stdout, "## Triggered by") > strings.Index(stdout, "## Answer") {
		t.Errorf("the trigger must precede the answer:\n%s", stdout)
	}
	// The excerpt is capped, and says so instead of pretending to be whole.
	if !strings.Contains(stdout, "excerpt") || strings.Contains(stdout, "line6") {
		t.Errorf("a long trigger must be excerpted and labeled as one:\n%s", stdout)
	}
}

// TestAgentStatusActiveShowsRecentAndWorking: while a turn runs there is no
// answer to show, so the third part becomes the last few messages plus a
// working indicator. Printing the previous turn's answer there is exactly the
// stale-result failure this design forbids.
func TestAgentStatusActiveShowsRecentAndWorking(t *testing.T) {
	statusStub(t, piRow(true, nil), statusMessagesJSON, "must not appear", "")
	stdout := captureStdout(t, func() {
		if code := cmdAgentStatus("abcd1234", false); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	for _, want := range []string{"alive, active; working", "## Recent", "[tool] bash", "still working"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("active status missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "## Answer") {
		t.Errorf("an active turn has no answer to report:\n%s", stdout)
	}
}

// TestAgentStatusJSONMirrorsTheThreeParts: the machine shape is one object with
// the same three parts, and content.kind is what a consumer switches on.
func TestAgentStatusJSONMirrorsTheThreeParts(t *testing.T) {
	statusStub(t, piRow(false, nil), statusMessagesJSON, "All green.", "")
	stdout := captureStdout(t, func() {
		if code := cmdAgentStatus("abcd1234", true); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	var rep struct {
		State struct {
			ShortID string `json:"short_id"`
			Adapter string `json:"adapter"`
			Alive   bool   `json:"alive"`
			Active  bool   `json:"active"`
			Outcome string `json:"last_turn_outcome"`
		} `json:"state"`
		Trigger struct {
			Text      string `json:"text"`
			Truncated bool   `json:"truncated"`
		} `json:"trigger"`
		Content struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not one JSON object: %v (%q)", err, stdout)
	}
	if rep.State.ShortID != "abcd1234" || rep.State.Adapter != "pi" || !rep.State.Alive || rep.State.Active {
		t.Errorf("state = %+v", rep.State)
	}
	if rep.State.Outcome != waitOutcomeCompleted {
		t.Errorf("last_turn_outcome = %q", rep.State.Outcome)
	}
	if !strings.HasPrefix(rep.Trigger.Text, "run the tests") || !rep.Trigger.Truncated {
		t.Errorf("trigger = %+v", rep.Trigger)
	}
	if rep.Content.Kind != agentStatusContentAnswer || rep.Content.Text != "All green." {
		t.Errorf("content = %+v", rep.Content)
	}
}

// TestAgentStatusIsAdapterGated: a session whose conversation gmux cannot read
// still gets the state line, which is adapter-independent, plus a note and the
// raw-view hint. Refusing the whole question because one part is unanswerable
// would be worse than answering the part that is — so this exits 0.
func TestAgentStatusIsAdapterGated(t *testing.T) {
	d := startStubDaemon(t, []cliSession{{ID: "sess-abcd1234", Adapter: "shell", Alive: true}})
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeErrEnvelope(w, http.StatusUnprocessableEntity, codeUnsupportedAdapter, `adapter "shell" does not render conversations`)
	})
	stdout := captureStdout(t, func() {
		if code := cmdAgentStatus("abcd1234", false); code != waitExitOK {
			t.Errorf("exit = %d, want 0 (the state line is still an answer)", code)
		}
	})
	for _, want := range []string{"## State", "abcd1234 shell", codeUnsupportedAdapter, "gmux tail abcd1234"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("gated status missing %q:\n%s", want, stdout)
		}
	}
}

// TestAgentStatusIsAStoreOnlySnapshot: it works on a dead retained session and
// issues nothing that could start, resume or drive one.
func TestAgentStatusIsAStoreOnlySnapshot(t *testing.T) {
	d := statusStub(t, nil, statusMessagesJSON, "All green.", "")
	d.mu.Lock()
	d.sessions = []cliSession{{ID: "sess-abcd1234", Adapter: "pi", Alive: false}}
	d.mu.Unlock()
	stdout := captureStdout(t, func() {
		if code := cmdAgentStatus("abcd1234", false); code != waitExitOK {
			t.Errorf("dead session: exit = %d, want 0", code)
		}
	})
	if !strings.Contains(stdout, "dead, idle") {
		t.Errorf("a dead session's state line must say so:\n%s", stdout)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.requests {
		if r.method != http.MethodGet {
			t.Errorf("status issued a %s to %s; the read must be store-only", r.method, r.path)
		}
		if strings.Contains(r.path, "/prompt") || strings.Contains(r.path, "/input") || strings.Contains(r.path, "/resume") {
			t.Errorf("status touched %s", r.path)
		}
	}
}

// TestAgentStatusReportsAnEmptySessionWithoutClaimingSilence: a session with
// nothing recorded yet gets the skeleton with the daemon's code as the note,
// never an empty Answer that reads as "the agent said nothing" — and never a
// claim about what was asked, which an unreadable tape cannot support.
func TestAgentStatusReportsAnEmptySessionWithoutClaimingSilence(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeErrEnvelope(w, http.StatusNotFound, codeNoConversation, "session has no conversation")
	})
	stdout := captureStdout(t, func() {
		if code := cmdAgentStatus("abcd1234", false); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	for _, want := range []string{"## Triggered by", codeNoConversation, "## Answer"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("empty-session status missing %q:\n%s", want, stdout)
		}
	}
}

// TestAgentStatusHelpTeachesTheCognitiveSplit: the page must teach the fixed
// shape, the snapshot property (and its staleness against a wait), the machine
// shape, and the answer-only read that replaced `agent output`.
func TestAgentStatusHelpTeachesTheCognitiveSplit(t *testing.T) {
	var b strings.Builder
	printAgentUsage(&b, "agent status")
	page := b.String()
	for _, want := range []string{
		"gmux agent status <id> [--json]",
		"## State", "## Triggered by", "## Answer", "## Recent",
		"SNAPSHOT", "staler",
		// The fourth state and the omit rule are contract, so the page teaches
		// them: a caller who reads only help must not expect "completed" for a
		// session that died mid-turn, nor an always-present outcome field.
		"runner died", "ABSENT",
		"gmux agent logs --agent -n 1 <id>",
		"Local sessions only",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("agent status page missing %q:\n%s", want, page)
		}
	}
	b.Reset()
	printAgentUsage(&b, "agent logs")
	logs := b.String()
	for _, want := range []string{"--user", "--agent", "--tool", "--all", "--json", "AFTER", "--thinking", "gmux agent status <id>"} {
		if !strings.Contains(logs, want) {
			t.Errorf("agent logs page missing %q:\n%s", want, logs)
		}
	}
}

// TestAgentStatusDeadMidTurnIsAnError is the honesty rule `gmux wait` already
// follows (wait_pure.go's terminalReason/diedConclusion): a runner that dies
// with its turn still open leaves Active=true and NO terminal flag in the store
// — deliberately preserved — and that turn did not complete, it will never
// finish. Reporting "last turn completed" for a killed agent is the most
// ordinary lie this verb could tell, and the two verbs must never disagree
// about one identical row.
//
// The classification must key on the row's Active flag BEFORE liveness: ANDing
// liveness in first is what fell through to "completed".
func TestAgentStatusDeadMidTurnIsAnError(t *testing.T) {
	statusStub(t, piRowAlive(false, map[string]any{"active": true}, map[string]any{
		"exited_at": "2020-01-01T00:00:00Z",
	}), statusMessagesJSON, "", codeNoMessage)
	stdout := captureStdout(t, func() {
		if code := cmdAgentStatus("abcd1234", false); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	if strings.Contains(stdout, "completed") {
		t.Errorf("a session that died mid-turn must not report a completed turn:\n%s", stdout)
	}
	if !strings.Contains(stdout, "dead, idle; the turn never finished (runner died)") {
		t.Errorf("state line must name the death:\n%s", stdout)
	}

	// And the machine shape agrees with `wait`: error, cause runner_died.
	stdout = captureStdout(t, func() { cmdAgentStatus("abcd1234", true) })
	var rep struct {
		State struct {
			Alive   bool   `json:"alive"`
			Active  bool   `json:"active"`
			Outcome string `json:"last_turn_outcome"`
			Cause   string `json:"last_turn_cause"`
		} `json:"state"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("decode: %v (%q)", err, stdout)
	}
	if rep.State.Alive || rep.State.Active {
		t.Errorf("a dead runner runs nothing: %+v", rep.State)
	}
	if rep.State.Outcome != waitOutcomeError || rep.State.Cause != causeRunnerDied {
		t.Errorf("state = %+v, want error/runner_died like gmux wait", rep.State)
	}
}

// TestAgentStatusActiveTurnHasNoOutcome: a running turn has not concluded, so
// neither rendering may attribute one — the human line already hid it, while
// the JSON published the PREVIOUS turn's flags as `last_turn_outcome`, which a
// script reads as this turn's verdict. Pinned for all three active rows.
func TestAgentStatusActiveTurnHasNoOutcome(t *testing.T) {
	for _, flags := range []map[string]any{
		{"active": true},
		{"active": true, "error": true},
		{"active": true, "interrupted": true},
	} {
		statusStub(t, piRowAlive(true, flags, nil), statusMessagesJSON, "must not appear", "")
		stdout := captureStdout(t, func() {
			if code := cmdAgentStatus("abcd1234", true); code != waitExitOK {
				t.Errorf("%v: exit = %d, want 0", flags, code)
			}
		})
		var rep struct {
			State map[string]any `json:"state"`
		}
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Fatalf("decode: %v (%q)", err, stdout)
		}
		if rep.State["active"] != true {
			t.Errorf("%v: state.active = %v, want true", flags, rep.State["active"])
		}
		if _, present := rep.State["last_turn_outcome"]; present {
			t.Errorf("%v: an active turn must publish no outcome, got %v", flags, rep.State["last_turn_outcome"])
		}
		if _, present := rep.State["last_turn_cause"]; present {
			t.Errorf("%v: an active turn must publish no cause", flags)
		}
		// The human line and the JSON must agree.
		human := captureStdout(t, func() { cmdAgentStatus("abcd1234", false) })
		if strings.Contains(human, "last turn ") {
			t.Errorf("%v: state line must not name a last turn while active:\n%s", flags, human)
		}
	}
}

// TestAgentStatusStateOmitsRatherThanEmpties: a never-prompted session has no
// concluded turn, so the outcome is ABSENT rather than "" — the empty string is
// not in the outcome vocabulary, and a consumer must not have to know it means
// "consult turn_recorded".
func TestAgentStatusStateOmitsRatherThanEmpties(t *testing.T) {
	statusStub(t, []map[string]any{{"id": "sess-abcd1234", "adapter": "pi", "alive": true}},
		statusMessagesJSON, "", codeNoMessage)
	stdout := captureStdout(t, func() { cmdAgentStatus("abcd1234", true) })
	var rep struct {
		State map[string]any `json:"state"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("decode: %v (%q)", err, stdout)
	}
	if _, present := rep.State["last_turn_outcome"]; present {
		t.Errorf("state = %v, want no outcome for a session with no turn on record", rep.State)
	}
	if rep.State["turn_recorded"] != false {
		t.Errorf("turn_recorded = %v, want false", rep.State["turn_recorded"])
	}
}

// TestAgentStatusTriggerSurvivesABusyTurn: the trigger is the newest USER
// boundary in the conversation, not a member of some last-N window. A turn that
// makes thirty tool calls used to push its own prompt out of the shared window,
// and the report then claimed nothing had ever been asked while printing that
// turn's activity — a self-contradiction inside one fixed-shape report.
func TestAgentStatusTriggerSurvivesABusyTurn(t *testing.T) {
	var tools []string
	for i := 0; i < 30; i++ {
		tools = append(tools, `{"role":"assistant","type":"tool","text":"[tool] bash {}","prose":""}`)
	}
	d := startStubDaemon(t, localSession())
	d.mu.Lock()
	d.rows = piRow(true, nil)
	d.mu.Unlock()
	var triggerQueries []string
	d.on(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("types") == agentLogTypeUser {
			// The daemon filters to user messages, then tails: the newest user
			// message is always in the answer, however busy the turn.
			triggerQueries = append(triggerQueries, r.URL.RawQuery)
			_, _ = w.Write([]byte(`{"ok":true,"data":{"messages":[{"role":"user","type":"user","text":"refactor the store","prose":"refactor the store"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"data":{"messages":[` + strings.Join(tools, ",") + `]}}`))
	})
	stdout := captureStdout(t, func() {
		if code := cmdAgentStatus("abcd1234", false); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	if !strings.Contains(stdout, "refactor the store") {
		t.Errorf("the trigger must survive a busy turn:\n%s", stdout)
	}
	if strings.Contains(stdout, "nothing has been asked") {
		t.Errorf("report contradicts itself (no trigger, yet activity):\n%s", stdout)
	}
	if len(triggerQueries) != 1 || !strings.Contains(triggerQueries[0], "tail=1") {
		t.Errorf("trigger read = %v, want one targeted types=user&tail=1 read", triggerQueries)
	}
}

// TestAgentStatusDoesNotClaimNothingWasAskedWhenItCannotRead: an unreadable
// conversation supports no claim about what was asked. "No user message in a
// readable transcript" and "no readable transcript" are different facts and the
// report keeps them apart.
func TestAgentStatusDoesNotClaimNothingWasAskedWhenItCannotRead(t *testing.T) {
	d := startStubDaemon(t, []cliSession{{ID: "sess-abcd1234", Adapter: "shell", Alive: true}})
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeErrEnvelope(w, http.StatusUnprocessableEntity, codeUnsupportedAdapter, `adapter "shell" does not render conversations`)
	})
	stdout := captureStdout(t, func() {
		if code := cmdAgentStatus("abcd1234", false); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	if strings.Contains(stdout, "nothing has been asked") {
		t.Errorf("an unreadable conversation cannot support that claim:\n%s", stdout)
	}
	if !strings.Contains(stdout, codeUnsupportedAdapter) {
		t.Errorf("the trigger part must carry the daemon's reason:\n%s", stdout)
	}

	// A readable transcript with no user message keeps the plain claim, and the
	// JSON marks the difference with the presence of trigger.code.
	statusStub(t, piRow(false, nil), `{"ok":true,"data":{"messages":[]}}`, "", codeNoMessage)
	d2Stdout := captureStdout(t, func() { cmdAgentStatus("abcd1234", false) })
	if !strings.Contains(d2Stdout, "nothing has been asked") {
		t.Errorf("a readable, prompt-free conversation is 'nothing has been asked':\n%s", d2Stdout)
	}
}

// TestAgentStatusReadsOneSessionListing: liveness and the turn flags are
// classified together, so they must come from ONE snapshot. Two listings let a
// session that dies in between be reported as alive with its post-death flags —
// the same liveness/turn-flag pairing the died-mid-turn rule depends on.
func TestAgentStatusReadsOneSessionListing(t *testing.T) {
	d := statusStub(t, piRow(false, nil), statusMessagesJSON, "All green.", "")
	captureStdout(t, func() { cmdAgentStatus("abcd1234", false) })
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sessionsRequests != 1 {
		t.Errorf("status issued %d session listings, want exactly 1", d.sessionsRequests)
	}
}
