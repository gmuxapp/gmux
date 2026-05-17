package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

// session is the subset of gmuxd's Session model that the CLI cares
// about. Defined locally to avoid pulling in the gmuxd store package.
type cliSession struct {
	ID         string `json:"id"`
	Peer       string `json:"peer,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	Kind       string `json:"kind"`
	Alive      bool   `json:"alive"`
	Pid        int    `json:"pid,omitempty"`
	Title      string `json:"title,omitempty"`
	Slug       string `json:"slug,omitempty"`
	SocketPath string `json:"socket_path,omitempty"`
	Command    []string `json:"command,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	ExitedAt   string `json:"exited_at,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
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
// the user's reference points to. host is the optional --host flag; an
// empty host means "local sessions only". See matchSession for the
// matching rules.
func resolveSession(ref, host string) (cliSession, error) {
	sessions, err := fetchSessions()
	if err != nil {
		return cliSession{}, err
	}
	return matchSession(sessions, ref, host)
}

// matchSession resolves a user-supplied reference to a single session,
// scoped by host.
//
// A reference can be:
//   - the full session ID ("sess-abcd1234") or full slug
//   - the short form shown by --list ("abcd1234", i.e. the ID with its
//     "sess-" prefix stripped)
//   - a unique prefix of any of the above
//   - any of the above with an "@<host>" suffix to target a peer
//
// host comes from the --host flag. An @-suffix in ref overrides host
// (or must agree if both are present). An empty effective host scopes
// the lookup to local sessions only; consistency over magic.
//
// Exact matches (on either ID or slug) always win, even when a shorter
// prefix would also match something else. Ambiguous prefixes return a
// human-readable error listing the candidates. As a friendly hint, if
// a strict-local lookup fails but exactly one peer session matches the
// ref, the error suggests the qualified id@peer form.
func matchSession(sessions []cliSession, ref, host string) (cliSession, error) {
	if ref == "" {
		return cliSession{}, fmt.Errorf("empty session reference")
	}

	// Split off any @host suffix. Reconcile with the --host flag.
	if idx := strings.LastIndex(ref, "@"); idx > 0 {
		suffixHost := ref[idx+1:]
		if host != "" && host != suffixHost {
			return cliSession{}, fmt.Errorf("--host=%q conflicts with %q in session ref", host, suffixHost)
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
	// exactly one peer session, suggest the qualified form rather than
	// just saying "not found". This is the most common confused-paste
	// case (`gmux --list --all` shows c0b3c1a1@konyvtar, user copies
	// just the c0b3c1a1).
	if host == "" {
		peerPool := make([]cliSession, 0, len(sessions))
		for _, s := range sessions {
			if s.Peer != "" {
				peerPool = append(peerPool, s)
			}
		}
		if hint, _ := lookupInPool(peerPool, ref); hint != nil {
			return cliSession{}, fmt.Errorf("session %q not found locally. Did you mean %s@%s?",
				ref, shortID(hint.ID), hint.Peer)
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
// --list shows), instead of dropping the @peer suffix and leaving them
// wondering which session the message refers to.
func displayID(s cliSession) string {
	if s.Peer == "" {
		return shortID(s.ID)
	}
	return shortID(s.ID) + "@" + s.Peer
}

// cmdList implements `gmux --list`.
//
// Defaults to local sessions only; pass --all to include every peer, or
// --host=<peer> to scope to one. The ID column carries the @host
// suffix for peer sessions so the displayed ID is a single copy-paste
// unit that works directly with --send, --kill, etc.
//
// Rows are grouped alive-first then by start time; columns are kept
// shallow (id, status, kind, title, cwd) so the output stays readable
// in a narrow terminal.
func cmdList(host string, all bool) int {
	sessions, err := fetchSessions()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}

	// Scope to the requested view:
	//   default → local only (Peer == "")
	//   --all   → everything
	//   --host  → just that peer
	// (parseCLI already rejects --host + --all.)
	switch {
	case host != "":
		sessions = filterByHost(sessions, host)
	case !all:
		sessions = filterByHost(sessions, "")
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
	idW, statusW, kindW := len("ID"), len("STATUS"), len("KIND")
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
			// `gmux --send` must work without further typing.
			id += "@" + s.Peer
		}
		title := s.Title
		if title == "" {
			title = strings.Join(s.Command, " ")
		}
		row := [5]string{id, status, s.Kind, title, s.Cwd}
		rows = append(rows, row)
		if n := len(row[0]); n > idW {
			idW = n
		}
		if n := len(row[1]); n > statusW {
			statusW = n
		}
		if n := len(row[2]); n > kindW {
			kindW = n
		}
	}

	fmt.Printf("%-*s  %-*s  %-*s  %s\n", idW, "ID", statusW, "STATUS", kindW, "KIND", "TITLE")
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %-*s  %-*s  %s", idW, r[0], statusW, r[1], kindW, r[2], r[3])
		if r[4] != "" {
			line += "  (" + r[4] + ")"
		}
		fmt.Println(line)
	}
	return 0
}

// cmdKill implements `gmux --kill <id>`.
//
// Routes through gmuxd rather than the session's own socket so remote
// peers work the same way local sessions do. gmuxd translates this into
// a SIGTERM on the child process and lets the normal exit lifecycle
// update the store.
func cmdKill(ref, host string) int {
	sess, err := resolveSession(ref, host)
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

// cmdTail implements `gmux --tail N <id>`.
//
// Routes through gmuxd's scrollback broker rather than the per-session
// Unix socket so the same code path serves four cases uniformly:
// local-live (broker reads from disk; the runner tees output there),
// local-dead (broker still reads from disk), peer-live (broker forwards
// to the owning gmuxd), and peer-dead (forward-then-disk on the peer).
// Output is raw PTY bytes including ANSI; pipe through your favorite
// stripper if you want plain text.
func cmdTail(ref string, n int, host string) int {
	sess, err := resolveSession(ref, host)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}

	client := gmuxdClient()
	url := fmt.Sprintf("%s/v1/sessions/%s/scrollback?tail=%d", gmuxdBaseURL(), sess.ID, n)
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusNotFound {
			fmt.Fprintf(os.Stderr, "gmux: session %s not found\n", displayID(sess))
			return 1
		}
		fmt.Fprintf(os.Stderr, "gmux: tail failed: %s: %s\n", resp.Status, strings.TrimSpace(string(body)))
		return 1
	}
	if _, err := io.Copy(os.Stdout, resp.Body); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	return 0
}

// cmdSend implements `gmux --send [--no-submit] <id> [text]`.
//
// Sends bytes to the session's PTY as if they had been typed at the
// terminal, then appends a carriage return so the input is submitted.
// The submit-by-default shape matches what every other "send a message"
// CLI does (gh issue comment, slack send, jira issue comment add) and
// avoids the silent-failure trap of bytes-without-\r sitting in the
// agent's input box forever. `--no-submit` opts out for the rare
// flow where you want to pre-fill input without dispatching it.
//
// When text is provided inline it is sent verbatim before the submit;
// when text is omitted, stdin is read until EOF — the natural shape
// for piping: `echo hello | gmux --send abc`.
//
// Routes through gmuxd's session-action API rather than dialing the
// runner socket directly, so the same code path handles local and
// peer sessions (gmuxd forwards to the owning peer transparently).
// Access control inherits from gmuxd: local IPC is owner-only, and
// peers honor their own `tailscale.allow` config.
func cmdSend(ref string, text *string, noSubmit bool, host string) int {
	sess, err := resolveSession(ref, host)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}

	body := buildSendBody(text, os.Stdin, noSubmit)

	client := gmuxdClient()
	// Stdin may be paced by a human or upstream process; the default
	// 5s timeout would cut off legitimately slow inputs. gmuxd buffers
	// the whole body before forwarding to the runner, so a long-lived
	// connection here is fine.
	client.Timeout = 0
	url := gmuxdBaseURL() + "/v1/sessions/" + sess.ID + "/input"
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
		default:
			fmt.Fprintf(os.Stderr, "gmux: send failed: %s: %s\n", resp.Status, strings.TrimSpace(string(msg)))
		}
		return 1
	}
	return 0
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
func buildSendBody(text *string, stdin io.Reader, noSubmit bool) io.Reader {
	var body io.Reader
	if text != nil {
		body = strings.NewReader(*text)
	} else {
		body = io.LimitReader(stdin, maxSendBytes)
	}
	if !noSubmit {
		body = io.MultiReader(body, strings.NewReader("\r"))
	}
	return body
}


