package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux/internal/localterm"
)

// session is the subset of gmuxd's Session model that the CLI cares
// about. Defined locally to avoid pulling in the gmuxd store package.
type cliSession struct {
	ID      string `json:"id"`
	Peer    string `json:"peer,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Adapter string `json:"adapter"`
	// DriveMode is how gmux hosts this harness (ADR 0033). Absent on the
	// wire means terminal; "acp" sessions have no PTY. Round-trips through
	// `gmux ls --json` so scripts can see the mode axis.
	DriveMode     string `json:"drive_mode,omitempty"`
	Alive         bool   `json:"alive"`
	Pid           int    `json:"pid,omitempty"`
	Title         string `json:"title,omitempty"`
	Slug          string `json:"slug,omitempty"`
	RunnerVersion string `json:"runner_version,omitempty"`
	// ParentSessionID links a session to the one it was spawned from
	// (e.g. `gmux edit` as $EDITOR inside a session). Must round-trip
	// through `gmux ls --json` for scripts to see the relationship.
	ParentSessionID string   `json:"parent_session_id,omitempty"`
	SocketPath      string   `json:"socket_path,omitempty"`
	Command         []string `json:"command,omitempty"`
	StartedAt       string   `json:"started_at,omitempty"`
	ExitedAt        string   `json:"exited_at,omitempty"`
	ExitCode        *int     `json:"exit_code,omitempty"`
}

// MarshalJSON adds the authoritative, directly reusable session reference to
// the daemon projection. ref is derived here rather than trusted from the API,
// so it cannot disagree with the id/peer decomposition.
func (s cliSession) MarshalJSON() ([]byte, error) {
	type wireSession cliSession
	return json.Marshal(struct {
		Ref string `json:"ref"`
		wireSession
	}{
		Ref:         displayID(s),
		wireSession: wireSession(s),
	})
}

// fetchSessions queries gmuxd for the full session list. Starts gmuxd
// if it's not already running so management commands work on a cold
// machine, the same way `gmux <cmd>` does.
func fetchSessions() ([]cliSession, error) {
	return fetchSessionsContext(context.Background())
}

func fetchSessionsContext(ctx context.Context) ([]cliSession, error) {
	ensureGmuxdContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client := gmuxdClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gmuxdBaseURL()+"/v1/sessions", nil)
	if err != nil {
		return nil, fmt.Errorf("build sessions request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact gmuxd: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gmuxd returned %s", resp.Status)
	}

	var envelope struct {
		OK   bool         `json:"ok"`
		Data []cliSession `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode sessions: %w", err)
	}
	for _, s := range envelope.Data {
		if s.Peer != "" && !validPeerName.MatchString(s.Peer) {
			return nil, fmt.Errorf("decode sessions: invalid peer name %q", s.Peer)
		}
	}
	return envelope.Data, nil
}

// resolveSession fetches the session list from gmuxd and finds the one
// the user's reference points to. See matchSession for the matching
// rules.
//
// A just-launched session may not yet be visible in the composed
// snapshot served by GET /v1/sessions (the compose loop runs
// asynchronously after the registration commit). To close this
// read-your-writes window, resolveSession retries a few times with
// short backoff when the first attempt yields "no session matches".
// The retry is bounded (≤600ms total) so interactive commands stay
// snappy, and only fires on a clean miss — ambiguous/malformed refs
// fail immediately.
func resolveSession(ref string) (cliSession, error) {
	return resolveSessionContext(context.Background(), ref)
}

func resolveSessionContext(ctx context.Context, ref string) (cliSession, error) {
	const (
		maxRetries = 6
		retryDelay = 100 * time.Millisecond
	)
	for attempt := 0; ; attempt++ {
		sessions, err := fetchSessionsContext(ctx)
		if err != nil {
			return cliSession{}, err
		}
		sess, err := matchSession(sessions, ref)
		if err == nil {
			return sess, nil
		}
		// Only retry on a clean miss ("no session matches"). Ambiguous
		// refs, empty refs, and peer-hint misses are not transient.
		if attempt >= maxRetries || !isNoMatchError(err) {
			return cliSession{}, err
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return cliSession{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// isNoMatchError returns true when the error is the specific "no
// session matches" sentinel from matchSession — the only case where
// a retry can help (the session exists in the store but the composed
// snapshot hasn't caught up yet).
func isNoMatchError(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "no session matches")
}

// matchSession resolves a user-supplied reference to a single session.
//
// A reference can be:
//   - the full 8-character session ID or full slug
//   - a unique prefix of either
//   - any of the above with an "@<peer>" suffix to target a peer
//
// Local-by-default (ADR 0009): without an @suffix the lookup is scoped
// to local sessions only, so a bare ref can never match — let alone act
// on — a session on another host. Addressing a peer requires explicitly
// typing "@<peer>".
//
// Exact matches (on either ID or slug) always win, even when a shorter
// prefix would also match something else. Ambiguous prefixes return a
// human-readable error listing the candidates. As a friendly hint, if
// a strict-local lookup fails but exactly one peer session matches the
// ref, the error suggests the qualified id@peer form.
func matchSession(sessions []cliSession, ref string) (cliSession, error) {
	if ref == "" {
		return cliSession{}, fmt.Errorf("empty session reference")
	}

	// The only way to widen the scope past local is an explicit @peer
	// suffix on the ref.
	host := ""
	if idx := strings.LastIndex(ref, "@"); idx > 0 {
		suffixHost := ref[idx+1:]
		if suffixHost == "" {
			// `id@` with no host is almost certainly a typo. Treating it
			// as local would silently scope wrong; demand the user make
			// the intent explicit.
			return cliSession{}, fmt.Errorf("session ref %q has empty @host suffix", ref)
		}
		host = suffixHost
		ref = ref[:idx]
	}

	// Filter to the pool of sessions on the requested host. Empty host
	// = local only (Peer == "").
	pool := filterByHost(sessions, host)
	match, candidates := lookupInPool(pool, ref)
	if match != nil {
		return *match, nil
	}
	if len(candidates) > 1 {
		ids := make([]string, 0, len(candidates))
		for _, c := range candidates {
			ids = append(ids, displayID(c))
		}
		return cliSession{}, fmt.Errorf("ambiguous session %q matches: %s", ref, strings.Join(ids, ", "))
	}

	// Friendly miss: if the user gave no host and the ref matches
	// peer sessions, suggest the qualified form rather than just
	// saying "not found". This is the most common confused-paste
	// case (`gmux ls --all` shows c0b3c1a1@konyvtar, user copies
	// just the c0b3c1a1).
	if host == "" {
		peerPool := make([]cliSession, 0, len(sessions))
		for _, s := range sessions {
			if s.Peer != "" {
				peerPool = append(peerPool, s)
			}
		}
		hint, peerCandidates := lookupInPool(peerPool, ref)
		switch {
		case hint != nil:
			return cliSession{}, fmt.Errorf("session %q not found locally. Did you mean %s?",
				ref, displayID(*hint))
		case len(peerCandidates) > 1:
			// More than one peer session matches: don't pick a
			// favorite, list them so the user knows exactly which
			// qualified forms work.
			qualified := make([]string, 0, len(peerCandidates))
			for _, c := range peerCandidates {
				qualified = append(qualified, displayID(c))
			}
			return cliSession{}, fmt.Errorf("session %q not found locally; matches peer sessions: %s",
				ref, strings.Join(qualified, ", "))
		}
	}

	// Generic miss: distinguish "no sessions at all on host X" from
	// "sessions exist but none match this ref".
	if len(pool) == 0 && host != "" {
		return cliSession{}, fmt.Errorf("no sessions known on peer %q", host)
	}
	return cliSession{}, fmt.Errorf("no session matches %q", ref)
}

// filterByHost returns sessions whose Peer field equals host. Empty
// host filters to local sessions (Peer == ""). Kept as a helper so
// callers don't repeat the strict-local rule.
func filterByHost(sessions []cliSession, host string) []cliSession {
	out := make([]cliSession, 0, len(sessions))
	for _, s := range sessions {
		if s.Peer == host {
			out = append(out, s)
		}
	}
	return out
}

// lookupInPool runs the two-pass exact-then-prefix match against pool.
// Returns:
//   - (&match, nil): unique resolution
//   - (nil, nil): no candidates at all
//   - (nil, candidates): more than one prefix match; caller surfaces
//     them as an ambiguity error
//
// Returning a pointer for the match keeps the "not found" sentinel
// distinct from a zero-value session (which is a valid match shape).
func lookupInPool(pool []cliSession, ref string) (*cliSession, []cliSession) {
	// Pass 1a: an exact immutable ID wins over every slug match.
	for i := range pool {
		if pool[i].ID == ref {
			return &pool[i], nil
		}
	}
	// Pass 1b: duplicate exact slugs are ambiguous rather than pool-order
	// dependent.
	var exactSlugs []cliSession
	for _, s := range pool {
		if s.Slug == ref {
			exactSlugs = append(exactSlugs, s)
		}
	}
	if len(exactSlugs) == 1 {
		return &exactSlugs[0], nil
	}
	if len(exactSlugs) > 1 {
		return nil, exactSlugs
	}

	// Pass 2: unique prefix match.
	var matches []cliSession
	for _, s := range pool {
		if strings.HasPrefix(s.ID, ref) || (s.Slug != "" && strings.HasPrefix(s.Slug, ref)) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	default:
		return nil, matches
	}
}

// Peer names are normalized at creation by both manual-peer and devcontainer
// discovery. Pin that grammar at the CLI boundary too: in particular, '@'
// cannot occur and make the authoritative id@peer reference ambiguous.
var validPeerName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// displayID returns the one canonical user-visible session address: the full
// ID, qualified with @peer when the session is remote.
func displayID(s cliSession) string {
	if s.Peer == "" {
		return s.ID
	}
	return s.ID + "@" + s.Peer
}

// cmdList implements `gmux ls [--all] [--json]`.
//
// Defaults to local sessions only; pass --all to include every peer.
// The ID column carries the @peer suffix for peer sessions so the
// displayed ID is a single copy-paste unit that works directly with
// send, kill, etc.
//
// Rows are grouped alive-first then by start time; columns are kept
// shallow (id, status, adapter, title, cwd) so the output stays readable
// in a narrow terminal.
func cmdList(all bool, asJSON bool) int {
	sessions, err := fetchSessions()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}

	// Scope to the requested view:
	//   default → local only (Peer == "")
	//   --all   → everything
	if !all {
		sessions = filterByHost(sessions, "")
	}

	// Both renderings have the same deterministic order: alive first, then
	// newest started_at, then canonical ref for equal/unknown timestamps.
	sortSessions(sessions)

	if asJSON {
		return emitSessionsJSON(sessions)
	}

	if len(sessions) == 0 {
		fmt.Println("no sessions")
		return 0
	}

	// Measure columns.
	idW, statusW, adapterW := len("ID"), len("STATUS"), len("ADAPTER")
	rows := make([][5]string, 0, len(sessions))
	for _, s := range sessions {
		status := "dead"
		if s.Alive {
			status = "alive"
		}
		id := s.ID
		if s.Peer != "" {
			// The @peer suffix is part of the addressable ID, not just
			// status flavor: copy-pasting this row's ID into
			// `gmux send` must work without further typing.
			id += "@" + s.Peer
		}
		title := s.Title
		if title == "" {
			title = strings.Join(s.Command, " ")
		}
		// The mode rides the adapter cell rather than adding a column: it
		// is exceptional (terminal is the default and stays unmarked), and
		// a copy-pasteable table should not grow a column for it.
		adapterCell := s.Adapter
		if s.DriveMode == "acp" {
			adapterCell += " (acp)"
		}
		row := [5]string{id, status, adapterCell, title, s.Cwd}
		rows = append(rows, row)
		if n := len(row[0]); n > idW {
			idW = n
		}
		if n := len(row[1]); n > statusW {
			statusW = n
		}
		if n := len(row[2]); n > adapterW {
			adapterW = n
		}
	}

	fmt.Printf("%-*s  %-*s  %-*s  %s\n", idW, "ID", statusW, "STATUS", adapterW, "ADAPTER", "TITLE")
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %-*s  %-*s  %s", idW, r[0], statusW, r[1], adapterW, r[2], r[3])
		if r[4] != "" {
			line += "  (" + r[4] + ")"
		}
		fmt.Println(line)
	}
	return 0
}

func sortSessions(sessions []cliSession) {
	sort.SliceStable(sessions, func(i, j int) bool {
		a, b := sessions[i], sessions[j]
		if a.Alive != b.Alive {
			return a.Alive
		}
		aTime, aErr := time.Parse(time.RFC3339Nano, a.StartedAt)
		bTime, bErr := time.Parse(time.RFC3339Nano, b.StartedAt)
		switch {
		case aErr == nil && bErr == nil && !aTime.Equal(bTime):
			return aTime.After(bTime)
		case (aErr == nil) != (bErr == nil):
			return aErr == nil
		case displayID(a) != displayID(b):
			return displayID(a) < displayID(b)
		default:
			return a.Adapter < b.Adapter
		}
	})
}

// cmdKill implements `gmux kill <id>`.
//
// Routes through gmuxd rather than the session's own socket so remote
// peers work the same way local sessions do. gmuxd translates this into
// a SIGTERM on the child process and lets the normal exit lifecycle
// update the store.
func cmdKill(ref string) int {
	sess, err := resolveSession(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	if !sess.Alive {
		fmt.Fprintf(os.Stderr, "gmux: session %s is already not running\n", displayID(sess))
		return 1
	}

	client := gmuxdClient()
	url := gmuxdBaseURL() + "/v1/sessions/" + sess.ID + "/kill"
	resp, err := client.Post(url, "application/json", strings.NewReader("{}"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "gmux: kill failed: %s: %s\n", resp.Status, strings.TrimSpace(string(body)))
		return 1
	}
	fmt.Printf("killed %s\n", displayID(sess))
	return 0
}

// cmdTail implements `gmux tail <id> [-n N]`: the last n lines of the
// session's terminal output, for any session, always.
//
// tail answers one question — "what is on its screen" — and answers it
// the same way for a shell, a one-shot command and an agent. The
// conversation-markdown view it defaulted to between 2.1 and this
// release now lives at `gmux agent logs` ("what has it been doing"),
// because a verb whose output shape depended on the session's adapter
// could not be scripted without first knowing what was running in it.
//
// Output is plain text: scrollback requests are rendered through a
// terminal emulator in the broker, so ANSI escapes never survive the
// server side, and stripANSI below is a belt-and-braces pass.
//
// Everything routes through gmuxd rather than the per-session Unix
// socket so the same code path serves local-live, local-dead,
// peer-live, and peer-dead uniformly (scrollback requests are
// forwarded to the owning gmuxd for peer sessions).
func cmdTail(ref string, n int) int {
	sess, err := resolveSession(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	data, code := fetchScrollback(sess, n)
	if code != 0 {
		return code
	}
	data = stripANSI(data)
	if _, err := os.Stdout.Write(data); err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	return 0
}

// errorCode extracts the machine-readable code from a gmuxd error
// envelope ({"ok":false,"error":{"code":...}}); "" if the body isn't one.
func errorCode(body []byte) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Error.Code
}

// fetchScrollback pulls the last n lines of a session's scrollback from
// gmuxd's broker. Returns the raw bytes and a process exit code (0 ok).
func fetchScrollback(sess cliSession, n int) ([]byte, int) {
	client := gmuxdClient()
	url := fmt.Sprintf("%s/v1/sessions/%s/scrollback?tail=%d", gmuxdBaseURL(), sess.ID, n)
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return nil, 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusNotFound {
			fmt.Fprintf(os.Stderr, "gmux: session %s not found\n", displayID(sess))
			return nil, 1
		}
		if resp.StatusCode == http.StatusUnprocessableEntity {
			// A capability refusal (e.g. an ACP session has no terminal
			// screen, ADR 0033) arrives with a self-contained message.
			if msg := extractMessage(body); msg != "" {
				fmt.Fprintf(os.Stderr, "gmux: %s\n", msg)
				return nil, 1
			}
		}
		fmt.Fprintf(os.Stderr, "gmux: tail failed: %s: %s\n", resp.Status, strings.TrimSpace(string(body)))
		return nil, 1
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return nil, 1
	}
	return data, 0
}

// cmdSend implements `gmux send <id> [text] [Key...]`.
//
// Sends bytes to the session's PTY as if typed at the terminal: the
// inline text (or piped stdin) followed by any trailing key tokens.
// Submission is explicit (ADR 0009) — a trailing `Enter` key, or a \r
// in piped bytes — so there is no implicit carriage return. send is
// raw and adapter-blind: nothing here depends on which agent runs in
// the session. Semantic, adapter-aware submission (steer a turn, queue
// a follow-up, interrupt) belongs to the agent layer of ADR 0027.
//
// When text is provided inline it is sent verbatim; when it is omitted
// and stdin is a pipe, stdin is read until EOF (`echo hi | gmux send
// <id> Enter`). Trailing keys (`Enter`, `C-c`, ...) render to their
// terminal byte sequences and are appended after the body.
//
// Routes through gmuxd's session-action API rather than dialing the
// runner socket directly, so the same code path handles local and
// peer sessions (gmuxd forwards to the owning peer transparently).
// Access control inherits from gmuxd: local IPC is owner-only, and
// peers honor their own `tailscale.allow` config.
func cmdSend(ref string, text *string, keys []string, wait bool, timeoutSecs int) int {
	sess, err := resolveSession(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}

	// Read stdin only when no inline text was given AND stdin is a pipe.
	// The tty guard is essential: without it, `gmux send <id> Enter` typed
	// interactively would block reading the terminal. With it, piped input
	// composes with trailing keys, so `echo hi | gmux send <id> Enter`
	// sends "hi" then submits instead of silently dropping "hi".
	var stdin io.Reader
	if text == nil && !localterm.IsInteractive() {
		stdin = os.Stdin
	}
	body := buildSendBody(text, keys, stdin)
	if !wait {
		return postInput(sess, body, "")
	}

	// `gmux send --wait`: one request delivers the input AND blocks
	// until the turn it triggers completes. gmuxd subscribes to session
	// events *before* forwarding the bytes to the runner, so unlike the
	// `gmux send X && gmux wait X` composition it cannot mistake the
	// previous turn's idle state for the reply (#218).
	//
	// Exit codes are `gmux wait`'s, through the same mapping (ADR 0027 §8):
	// 0 the turn completed, 2 it was intentionally interrupted, 1 anything
	// else — a failed turn, a death, a timeout, a usage/transport error.
	// Unlike `gmux wait`, no result is printed: raw input makes no claim
	// about which agent turn the bytes belong to.
	if sess.Peer != "" {
		// Same scope rule as `gmux wait`: the wait half needs the
		// owning daemon's event stream, which peers don't expose to
		// the CLI yet. Bare shortID: the message names the peer itself.
		fmt.Fprintf(os.Stderr, "gmux: send --wait is only supported for local sessions (%s is on peer %q)\n",
			sess.ID, sess.Peer)
		return 1
	}
	query := "?wait=idle"
	if timeoutSecs > 0 {
		query += "&timeout=" + strconv.Itoa(timeoutSecs)
	}
	return postInput(sess, body, query)
}

// cmdSendKeys implements the tmux-compatible `gmux send-keys -t <id>
// <keys...>` form: every argument is a key name unless -l (literal)
// is set, in which case arguments are sent as literal text.
func cmdSendKeys(ref string, keys []string, literal bool) int {
	return sendBytes(ref, strings.NewReader(renderKeys(keys, literal)))
}

// sendBytes resolves ref and POSTs body to the session's input endpoint.
func sendBytes(ref string, body io.Reader) int {
	sess, err := resolveSession(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	return postInput(sess, body, "")
}

// postInput POSTs body to the session's input endpoint. With an empty
// query this is fire-and-forget (204). With "?wait=idle..." gmuxd
// holds the response until the triggered turn completes and answers
// with a wait-style reason payload, mapped to `gmux wait` exit codes.
func postInput(sess cliSession, body io.Reader, query string) int {
	client := gmuxdClient()
	// Stdin may be paced by a human or upstream process; the default
	// 5s timeout would cut off legitimately slow inputs. gmuxd buffers
	// the whole body before forwarding to the runner, so a long-lived
	// connection here is fine.
	client.Timeout = 0
	url := gmuxdBaseURL() + "/v1/sessions/" + sess.ID + "/input" + query
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		switch resp.StatusCode {
		case http.StatusNotFound:
			fmt.Fprintf(os.Stderr, "gmux: session %s not found\n", displayID(sess))
		case http.StatusConflict:
			fmt.Fprintf(os.Stderr, "gmux: session %s is not running\n", displayID(sess))
		case http.StatusRequestTimeout:
			fmt.Fprintln(os.Stderr, "gmux: session did not become idle within --timeout")
			// A timeout is an error under the global taxonomy (ADR 0027 §8);
			// it no longer has a code of its own.
			return waitExitError
		case http.StatusUnprocessableEntity:
			fmt.Fprintf(os.Stderr, "gmux: %s\n", extractMessage(msg))
		default:
			fmt.Fprintf(os.Stderr, "gmux: send failed: %s: %s\n", resp.Status, strings.TrimSpace(string(msg)))
		}
		return 1
	}
	if query == "" {
		return 0
	}
	// wait=idle response: the same conclusion payload `gmux wait` gets
	// ({reason, outcome, cause}), minus any result. Mapping it with the same
	// function is what keeps an intentionally interrupted turn from exiting 0
	// through this path while `gmux wait` exits 2 for the identical turn.
	// io.Discard + quiet is not a stylistic choice: it is how this path stays
	// result-free while sharing every exit decision.
	var env struct {
		Data waitResult `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		fmt.Fprintln(os.Stderr, "gmux: decode send --wait response:", err)
		return 1
	}
	return reportWaitResult(env.Data, false, true, io.Discard)
}

// maxSendBytes caps the number of bytes read from stdin for a single
// --send invocation. Matches the runner's maxInputBytes so we fail
// fast on the client side rather than letting the server truncate us.
const maxSendBytes = 1 << 20 // 1 MiB

// buildSendBody assembles the bytes to write to the session PTY: the
// message body (inline text, else piped stdin if provided) followed by
// any trailing key sequences. stdin is nil unless the caller determined
// it is a pipe to read (see cmdSend). Submission is explicit — a
// trailing Enter key or a \r in the piped bytes; send never appends one.
func buildSendBody(text *string, keys []string, stdin io.Reader) io.Reader {
	readers := make([]io.Reader, 0, 2)
	switch {
	case text != nil:
		readers = append(readers, strings.NewReader(*text))
	case stdin != nil:
		readers = append(readers, io.LimitReader(stdin, maxSendBytes))
	}
	if len(keys) > 0 {
		readers = append(readers, strings.NewReader(renderKeys(keys, false)))
	}
	return io.MultiReader(readers...)
}

// emitSessionsJSON prints sessions as a single JSON array with a stable
// schema so agents can consume `gmux ls --json` without scraping the
// human table. Always emits an array (never null) even when empty.
func emitSessionsJSON(sessions []cliSession) int {
	if sessions == nil {
		sessions = []cliSession{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(sessions); err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	return 0
}

// stripANSI removes ANSI/VT escape sequences from PTY output so the
// scrollback view of `gmux tail` is grep-friendly text. The broker already
// renders tail responses through a terminal emulator, so this is
// belt-and-braces for whatever survives that pass. Reuse the foreground
// relay's parser so tail and `gmux -- <cmd>` cannot drift in what they strip.
func stripANSI(b []byte) []byte {
	var out strings.Builder
	_, _ = newANSIStrippingWriter(&out).Write(b) // strings.Builder never fails
	return []byte(out.String())
}
