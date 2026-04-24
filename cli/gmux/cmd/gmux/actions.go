package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
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
// the user's reference points to. See matchSession for the matching rules.
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
//   - the short form shown by --list ("abcd1234", i.e. the ID with its
//     "sess-" prefix stripped)
//   - a unique prefix of any of the above
//
// Exact matches (on either ID or slug) always win, even when a shorter
// prefix would also match something else. Ambiguous prefixes return a
// human-readable error listing the candidates.
func matchSession(sessions []cliSession, ref string) (cliSession, error) {
	if ref == "" {
		return cliSession{}, fmt.Errorf("empty session reference")
	}

	// Pass 1: exact matches take precedence, so "abcd" never accidentally
	// resolves to a prefix when a session is literally named "abcd".
	for _, s := range sessions {
		if s.ID == ref || s.Slug == ref || shortID(s.ID) == ref {
			return s, nil
		}
	}

	// Pass 2: unique prefix match. We also match against the short form
	// so "abcd" (a prefix of the short id shown by --list) resolves
	// as users expect, not only "sess-abcd...".
	var matches []cliSession
	for _, s := range sessions {
		switch {
		case strings.HasPrefix(s.ID, ref),
			strings.HasPrefix(shortID(s.ID), ref),
			s.Slug != "" && strings.HasPrefix(s.Slug, ref):
			matches = append(matches, s)
		}
	}

	switch len(matches) {
	case 0:
		return cliSession{}, fmt.Errorf("no session matches %q", ref)
	case 1:
		return matches[0], nil
	default:
		var ids []string
		for _, m := range matches {
			ids = append(ids, shortID(m.ID))
		}
		return cliSession{}, fmt.Errorf("ambiguous session %q matches: %s", ref, strings.Join(ids, ", "))
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

// cmdList implements `gmux --list`.
//
// Prints one row per session, grouped alive-first then by start time.
// The columns are kept intentionally shallow (id, status, kind, title,
// cwd) so the output stays readable in a narrow terminal; anyone who
// wants richer data can open the UI.
func cmdList() int {
	sessions, err := fetchSessions()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
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
		if s.Peer != "" {
			status += "@" + s.Peer
		}
		title := s.Title
		if title == "" {
			title = strings.Join(s.Command, " ")
		}
		row := [5]string{shortID(s.ID), status, s.Kind, title, s.Cwd}
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
func cmdKill(ref string) int {
	sess, err := resolveSession(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	if !sess.Alive {
		fmt.Fprintf(os.Stderr, "gmux: session %s is already not running\n", shortID(sess.ID))
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
	fmt.Printf("killed %s\n", shortID(sess.ID))
	return 0
}

// cmdTail implements `gmux --tail N <id>`.
//
// Reads from the session's own Unix socket (/scrollback/tail) rather
// than going through gmuxd: the socket path is already in the gmuxd
// session record, and this keeps the scrollback data off the daemon
// path. Remote peer sessions (no local socket_path) are rejected with
// a clear error rather than silently falling back to something lossy.
func cmdTail(ref string, n int) int {
	sess, err := resolveSession(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	if sess.SocketPath == "" {
		switch {
		case sess.Peer != "":
			fmt.Fprintf(os.Stderr, "gmux: --tail is only supported for local sessions (%s is on peer %q)\n",
				shortID(sess.ID), sess.Peer)
		default:
			fmt.Fprintf(os.Stderr, "gmux: session %s has no socket path\n", shortID(sess.ID))
		}
		return 1
	}

	// Live path: try the session's socket first. For alive sessions
	// this is the authoritative source, and for just-exited sessions
	// the socket may still be serving briefly.
	if body, ok := fetchLiveTail(sess.SocketPath, n); ok {
		os.Stdout.Write(body)
		return 0
	}

	// Disk fallback: ptyserver flushes the scrollback to
	// <socket>.tail on exit, so we can still show it for dead sessions.
	// This is what makes `gmux --tail` useful for post-hoc peeking at a
	// build or test run that has already finished.
	body, err := readPersistedTail(sess.SocketPath, n)
	if err == nil {
		os.Stdout.Write(body)
		return 0
	}
	if os.IsNotExist(err) {
		if sess.Alive {
			fmt.Fprintf(os.Stderr, "gmux: session %s is unreachable (socket refused, no persisted tail on disk)\n", shortID(sess.ID))
		} else {
			fmt.Fprintf(os.Stderr, "gmux: session %s has no persisted tail (started by an older runner, or tail file was removed)\n", shortID(sess.ID))
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, "gmux:", err)
	return 1
}

// fetchLiveTail pulls the last n lines from the session's live socket.
// Returns (body, true) on success, (nil, false) on any error that
// looks like "session is gone" (dial refused, connection reset) so the
// caller can fall back to disk. Non-gone errors (e.g. a 5xx from a
// live runner) are also reported as ok=false but logged; callers that
// want the detail should run with GMUX_DEBUG.
func fetchLiveTail(socketPath string, n int) ([]byte, bool) {
	client := sessionSocketClient(socketPath)
	url := fmt.Sprintf("http://session/scrollback/tail?n=%d", n)
	resp, err := client.Get(url)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 404 from an old runner: prefer the disk fallback silently,
		// let it decide whether to print the "too old" hint.
		return nil, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false
	}
	return body, true
}

// readPersistedTail returns the last n plain-text lines of the
// scrollback file written by ptyserver when the session exited.
// The file path is derived from the session's socket path; see
// ptyserver.persistTail for the writer side.
func readPersistedTail(socketPath string, n int) ([]byte, error) {
	data, err := os.ReadFile(socketPath + ".tail")
	if err != nil {
		return nil, err
	}
	return lastNLines(data, n), nil
}

// lastNLines returns the last n newline-separated lines of buf,
// preserving any trailing newline. When buf has fewer than n lines the
// whole buffer is returned. A buf without a trailing newline is still
// treated as ending with a line, so content from a truncated write
// isn't silently dropped.
//
// Implementation: walk backwards counting newline boundaries. With a
// trailing newline, seeing n+1 newlines from the end means buf[i+1:]
// starts at the n-th line from the end. Without one, the final
// unterminated line itself is the first line-from-end, so n newlines
// is enough.
func lastNLines(buf []byte, n int) []byte {
	if n <= 0 || len(buf) == 0 {
		return nil
	}
	end := len(buf)
	target := n + 1
	if buf[end-1] != '\n' {
		target = n
	}
	count := 0
	for i := end - 1; i >= 0; i-- {
		if buf[i] == '\n' {
			count++
			if count == target {
				return buf[i+1:]
			}
		}
	}
	return buf
}

// cmdSend implements `gmux --send <id> [text]`.
//
// Sends bytes to the session's PTY as if they had been typed at the
// terminal. When text is provided inline it is sent verbatim (callers
// who want a trailing newline must include it explicitly, e.g. via
// `$'hello\n'`). When text is omitted, stdin is read until EOF; this
// is the natural shape for piping: `echo hello | gmux --send abc`.
//
// Access control is delegated to the session socket's file permissions
// (owner-only, 0o700): if you can connect to the socket you already
// own the process and could do worse things without going through us.
// For the same reason we don't support sending to remote peer sessions
// — doing that safely would require a per-peer authorization model
// that doesn't exist yet.
func cmdSend(ref string, text *string) int {
	sess, err := resolveSession(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	if sess.SocketPath == "" {
		switch {
		case sess.Peer != "":
			fmt.Fprintf(os.Stderr, "gmux: --send is only supported for local sessions (%s is on peer %q)\n",
				shortID(sess.ID), sess.Peer)
		case !sess.Alive:
			fmt.Fprintf(os.Stderr, "gmux: session %s is not running\n", shortID(sess.ID))
		default:
			fmt.Fprintf(os.Stderr, "gmux: session %s has no socket path\n", shortID(sess.ID))
		}
		return 1
	}
	if !sess.Alive {
		fmt.Fprintf(os.Stderr, "gmux: session %s is not running\n", shortID(sess.ID))
		return 1
	}

	var body io.Reader
	if text != nil {
		body = strings.NewReader(*text)
	} else {
		body = io.LimitReader(os.Stdin, maxSendBytes)
	}

	client := sessionSocketClient(sess.SocketPath)
	// When reading from stdin, the request body may be paced by the
	// user or another process; the default 5s client timeout would cut
	// off legitimately slow inputs. Since the socket is local and the
	// handler just writes to the PTY, it's fine to let the call run
	// for as long as stdin keeps producing bytes.
	client.Timeout = 0
	req, err := http.NewRequest(http.MethodPost, "http://session/input", body)
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
		if resp.StatusCode == http.StatusNotFound {
			fmt.Fprintln(os.Stderr, "gmux: this session's runner is too old for --send (restart it to upgrade)")
			return 1
		}
		fmt.Fprintf(os.Stderr, "gmux: send failed: %s: %s\n", resp.Status, strings.TrimSpace(string(msg)))
		return 1
	}
	return 0
}

// maxSendBytes caps the number of bytes read from stdin for a single
// --send invocation. Matches the runner's maxInputBytes so we fail
// fast on the client side rather than letting the server truncate us.
const maxSendBytes = 1 << 20 // 1 MiB

// sessionSocketClient builds an HTTP client that dials a session's
// Unix socket directly. The host portion of URLs passed through this
// client is ignored — we use "session" by convention.
func sessionSocketClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
}
