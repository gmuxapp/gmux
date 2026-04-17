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
		case !sess.Alive:
			fmt.Fprintf(os.Stderr, "gmux: session %s is not running\n", shortID(sess.ID))
		default:
			fmt.Fprintf(os.Stderr, "gmux: session %s has no socket path\n", shortID(sess.ID))
		}
		return 1
	}

	client := sessionSocketClient(sess.SocketPath)
	url := fmt.Sprintf("http://session/scrollback/tail?n=%d", n)
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		// Older runners may not have the tail endpoint yet; give a
		// specific hint instead of a bare 404.
		if resp.StatusCode == http.StatusNotFound {
			fmt.Fprintln(os.Stderr, "gmux: this session's runner is too old for --tail (restart it to upgrade)")
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
