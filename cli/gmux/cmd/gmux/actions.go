package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/gmuxapp/gmux/cli/gmux/internal/localterm"
)

// session is the subset of gmuxd's Session model that the CLI cares
// about. Defined locally to avoid pulling in the gmuxd store package.
type cliSession struct {
	ID      string `json:"id"`
	Peer    string `json:"peer,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Adapter string `json:"adapter"`
	Alive   bool   `json:"alive"`
	Pid     int    `json:"pid,omitempty"`
	Title   string `json:"title,omitempty"`
	Slug    string `json:"slug,omitempty"`
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

// fetchSessions queries gmuxd for the full session list. Starts gmuxd
// if it's not already running so management commands work on a cold
// machine, the same way `gmux <cmd>` does.
func fetchSessions() ([]cliSession, error) {
	ensureGmuxd()
	client := gmuxdClient()
	resp, err := client.Get(gmuxdBaseURL() + "/v1/sessions")
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
	return envelope.Data, nil
}

// resolveSession fetches the session list from gmuxd and finds the one
// the user's reference points to. See matchSession for the matching
// rules.
func resolveSession(ref string) (cliSession, error) {
	sessions, err := fetchSessions()
	if err != nil {
		return cliSession{}, err
	}
	return matchSession(sessions, ref)
}

// matchSession resolves a user-supplied reference to a single session.
//
// A reference can be:
//   - the full session ID ("sess-abcd1234") or full slug
//   - the short form shown by `gmux ls` ("abcd1234", i.e. the ID with
//     its "sess-" prefix stripped)
//   - a unique prefix of any of the above
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
			ids = append(ids, shortID(c.ID))
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
			return cliSession{}, fmt.Errorf("session %q not found locally. Did you mean %s@%s?",
				ref, shortID(hint.ID), hint.Peer)
		case len(peerCandidates) > 1:
			// More than one peer session matches: don't pick a
			// favorite, list them so the user knows exactly which
			// qualified forms work.
			qualified := make([]string, 0, len(peerCandidates))
			for _, c := range peerCandidates {
				qualified = append(qualified, shortID(c.ID)+"@"+c.Peer)
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
	// Pass 1: exact matches take precedence.
	for i := range pool {
		s := pool[i]
		if s.ID == ref || s.Slug == ref || shortID(s.ID) == ref {
			return &s, nil
		}
	}

	// Pass 2: unique prefix match.
	var matches []cliSession
	for _, s := range pool {
		switch {
		case strings.HasPrefix(s.ID, ref),
			strings.HasPrefix(shortID(s.ID), ref),
			s.Slug != "" && strings.HasPrefix(s.Slug, ref):
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

// shortID returns the 8-char display form of a session id, matching
// what the web UI uses.
func shortID(id string) string {
	const prefix = "sess-"
	trimmed := strings.TrimPrefix(id, prefix)
	if len(trimmed) > 8 {
		trimmed = trimmed[:8]
	}
	return trimmed
}

// displayID returns the user-visible address for a session: shortID for
// local sessions, shortID@peer for peer sessions. Use it in error
// messages so the printed id matches what the user typed (and what
// `gmux ls` shows), instead of dropping the @peer suffix and leaving them
// wondering which session the message refers to.
func displayID(s cliSession) string {
	if s.Peer == "" {
		return shortID(s.ID)
	}
	return shortID(s.ID) + "@" + s.Peer
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

	if asJSON {
		return emitSessionsJSON(sessions)
	}

	if len(sessions) == 0 {
		fmt.Println("no sessions")
		return 0
	}

	// Alive first; within each group, newest first.
	sort.SliceStable(sessions, func(i, j int) bool {
		if sessions[i].Alive != sessions[j].Alive {
			return sessions[i].Alive
		}
		return sessions[i].StartedAt > sessions[j].StartedAt
	})

	// Measure columns.
	idW, statusW, adapterW := len("ID"), len("STATUS"), len("ADAPTER")
	rows := make([][5]string, 0, len(sessions))
	for _, s := range sessions {
		status := "dead"
		if s.Alive {
			status = "alive"
		}
		id := shortID(s.ID)
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
		row := [5]string{id, status, s.Adapter, title, s.Cwd}
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

// cmdTail implements `gmux tail <id> [-n N] [--raw]`.
//
// Routes through gmuxd's scrollback broker rather than the per-session
// Unix socket so the same code path serves four cases uniformly:
// local-live (broker reads from disk; the runner tees output there),
// local-dead (broker still reads from disk), peer-live (broker forwards
// to the owning gmuxd), and peer-dead (forward-then-disk on the peer).
// Output is raw PTY bytes including ANSI; pipe through your favorite
// stripper if you want plain text.
func cmdTail(ref string, n int, raw bool) int {
	sess, err := resolveSession(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}

	data, code := fetchScrollback(sess, n)
	if code != 0 {
		return code
	}
	if !raw {
		data = stripANSI(data)
	}
	if _, err := os.Stdout.Write(data); err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	return 0
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
// in piped bytes — so there is no implicit carriage return.
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
	if wait {
		return sendBytesAndWait(ref, body, timeoutSecs)
	}
	return sendBytes(ref, body)
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

// sendBytesAndWait implements `gmux send --wait`: one request delivers
// the input AND blocks until the turn it triggers completes. gmuxd
// subscribes to session events *before* forwarding the bytes to the
// runner, so unlike the `gmux send X && gmux wait X` composition it
// cannot mistake the previous turn's idle state for the reply (#218).
//
// Exit codes mirror `gmux wait`: 0 idle, 2 died, 3 timeout, 1 usage/
// transport errors.
func sendBytesAndWait(ref string, body io.Reader, timeoutSecs int) int {
	sess, err := resolveSession(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	if sess.Peer != "" {
		// Same scope rule as `gmux wait`: the wait half needs the
		// owning daemon's event stream, which peers don't expose to
		// the CLI yet. Bare shortID: the message names the peer itself.
		fmt.Fprintf(os.Stderr, "gmux: send --wait is only supported for local sessions (%s is on peer %q)\n",
			shortID(sess.ID), sess.Peer)
		return 1
	}
	query := "?wait=idle"
	if timeoutSecs > 0 {
		query += "&timeout=" + strconv.Itoa(timeoutSecs)
	}
	return postInput(sess, body, query)
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
			return waitExitTimeout
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
	// wait=idle response: { ok: true, data: { reason: "idle" | "died" } }
	var env struct {
		Data struct {
			Reason string `json:"reason"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		fmt.Fprintln(os.Stderr, "gmux: decode send --wait response:", err)
		return 1
	}
	switch env.Data.Reason {
	case "idle":
		return waitExitOK
	case "died":
		fmt.Fprintf(os.Stderr, "gmux: session %s died before becoming idle\n", displayID(sess))
		return waitExitDied
	default:
		fmt.Fprintf(os.Stderr, "gmux: unexpected send --wait reason %q\n", env.Data.Reason)
		return 1
	}
}

// maxSendBytes caps the number of bytes read from stdin for a single
// --send invocation. Matches the runner's maxInputBytes so we fail
// fast on the client side rather than letting the server truncate us.
const maxSendBytes = 1 << 20 // 1 MiB

// buildSendBody assembles the bytes that --send writes to the session's
// PTY input. When text is non-nil it is sent verbatim; otherwise stdin
// is read up to maxSendBytes. Unless noSubmit is set, a trailing \r is
// appended — that's what xterm sends for Enter and what every PTY
// (agent or shell) treats as "submit this line." \n alone is not
// enough: most agents see it as a literal newline and keep buffering,
// which is how `gmux --send <id> < prompt.txt` ended up silently
// failing for users who expected `cat`-style behavior. A redundant \r
// in the input is harmless (submits an empty line at most), so we don't
// try to detect and dedupe.
// buildSendBody assembles the bytes to write to the session PTY: the
// message body (inline text, else piped stdin if provided) followed by
// any trailing key sequences. stdin is nil unless the caller determined
// it is a pipe to read (see cmdSend). Submission is explicit — a
// trailing Enter key, or a \r in the piped bytes.
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
// default `gmux tail` is grep-friendly plain text. `--raw` bypasses
// this. Handles CSI (ESC [ ... letter), OSC (ESC ] ... BEL/ST), and
// lone two-byte escapes; this is a pragmatic stripper, not a full
// terminal emulator.
func stripANSI(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c != 0x1b { // not ESC
			if c != '\r' { // collapse bare CRs that survive re-render
				out = append(out, c)
			}
			i++
			continue
		}
		// ESC sequence.
		if i+1 >= len(b) {
			break
		}
		switch b[i+1] {
		case '[': // CSI: ESC [ params... final-byte (0x40-0x7e)
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			i = j + 1
		case ']': // OSC: ESC ] ... (BEL | ESC \)
			j := i + 2
			for j < len(b) && b[j] != 0x07 {
				if b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\' {
					j++
					break
				}
				j++
			}
			i = j + 1
		default: // two-byte escape
			i += 2
		}
	}
	return out
}
